package gateway

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// Claude Code fits an image it reads from a file before it sends one
// (Claude Code 2.1.316: 2000 px a side and 5 MiB of base64 at most), as it
// does with the files magpie pastes into its prompt. An image magpie hands
// it as an image block is sent as it is: the earlier turns' images in a
// transcript a run resumes (replaySession), a -p run's stream-json
// message, and, fitted here too, a tool's images in its MCP result. The API refuses an image over 8000 px a side, and Claude Code
// then says "API Error: an image in the conversation could not be
// processed and was removed" and the turn fails. DeepSeek Harness (dsh)
// sends images up to 8192 px a side (its normalizedImageMaxDimension, the
// long edge after a 2048x2048 pixel budget), so a tall screenshot of
// 400x8192 went through dsh and the -anthropic route to a Claude
// subscription and was refused (sean_80289 on Discord). Claude Code took
// the same image pasted, fitted to 2000 px. The API also holds every image
// of a request with more than 20 to 2000 px, which dsh's 2048 budget
// passes too.
//
// claudeImage gives an image block what Claude Code would have made of
// the image: scaled to fit 2000 px a side, a JPEG written again as JPEG at
// quality 85 and any other as PNG, then as JPEG at a lower quality if that
// is still over the base64 limit. An image that fits goes as it came, byte
// for byte. A BMP within the conversion limit goes as PNG. A TIFF, a HEIC
// the gateway did not convert at the door, any other format, or an
// incomplete or unreadable image goes as a note. Complete supported
// containers Go cannot decode go to Anthropic unchanged, with their real
// type, except for unsafe WebP layouts and images over the conversion
// budget. Complete GIF/WebP images that fit are not decoded. A busy decode
// goes as a note and is tried again on the next turn.
const (
	claudeImageSide   = 2000
	claudeImageBase64 = 5 << 20
)

// BMP keeps its pixel limit even when its decode, refit and encode fit
// the shared byte budget. The budget does not count the source bytes.
const claudeImageConvertPixels = 3840 * 2160

// Decodes share a byte budget through refit and encoding: decoder buffers,
// resize copies, weights and scratch, and bounded encoder output/state.
// It excludes raw. An image over capacity becomes a note before conversion.
// After a wait gives up, subsequent waits are short for ConvertWait, so a
// replay does not wait the full timeout for every image. Waiting is not fair:
// a large weight can starve behind small ones until its wait ends, as intended.
// Complete GIF/WebP images needing no refit do not decode or take the budget.
var (
	claudeImageDecodeBudget claudeImageByteBudget
	claudeImageConvertWait  = 20 * time.Second
	claudeImageBusyWait     = time.Second
)

type claudeImageByteBudget struct {
	mu      sync.Mutex
	used    int
	changed chan struct{}
	gaveUp  time.Time
}

func (b *claudeImageByteBudget) acquire(n int, wait time.Duration) bool {
	if n <= 0 || n > codexImageMaxBytes {
		return false
	}
	b.mu.Lock()
	if time.Since(b.gaveUp) < claudeImageConvertWait {
		wait = min(wait, claudeImageBusyWait)
	}
	b.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		b.mu.Lock()
		if n <= codexImageMaxBytes-b.used {
			b.used += n
			b.mu.Unlock()
			return true
		}
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			b.mu.Lock()
			b.gaveUp = time.Now()
			b.mu.Unlock()
			return false
		}
	}
}

func (b *claudeImageByteBudget) release(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if b.changed != nil {
		close(b.changed)
		b.changed = nil
	}
}

// claudeImageDecodeWeight bounds decode plus all refit attempts, not raw.
// codex_images.go:triangleResize holds a 4*sw*sh RGBA copy (count it even
// when the decoder could return RGBA), a 16*w*sh float buffer and 4*w*h
// output. triangleWeights' taps and growing float slices use at most
// 64*(sw+sh)+192*(w+h), including discarded capacities and allocation rounding.
// Refit may also flatten to 4*w*h RGBA. The writer reserves Output bytes,
// and copies at most Output bytes into the cache so a small result does not
// retain a large backing array. png/writer.go:writeImage uses six filter
// rows of at most 8*w+1 bytes. flate/deflate.go:compressor, initDeflate and
// huffman_bit_writer.go use <1 MiB (hash tables 640 KiB, window 64 KiB,
// tokens 64 KiB, Huffman tables); 2 MiB covers these, PNG's 32 KiB writer,
// JPEG writer.go:encoder's blocks/bufio and all four encoding attempts.
func claudeImageDecodeWeight(cfg image.Config, holds int, refit bool) int {
	if holds >= holdsMost {
		return holdsMost
	}
	cost := int64(holds)
	if refit {
		sw, sh := int64(cfg.Width), int64(cfg.Height)
		w, h := cfg.Width, cfg.Height
		if scale := math.Min(float64(claudeImageSide)/float64(max(w, h)), 1); scale < 1 {
			w = max(int(math.Round(float64(w)*scale)), 1)
			h = max(int(math.Round(float64(h)*scale)), 1)
			cost += 4*sw*sh + 16*int64(w)*sh + 64*(sw+sh) + 192*int64(w+h)
			cost += 4 * int64(w) * int64(h)
		} else {
			// png/writer.go:writeImage and jpeg/writer.go:writeSOS may
			// box colours from 16-bit/CMYK images when no resize made RGBA.
			cost += 16 * int64(w) * int64(h)
		}
		cost += 4*int64(w)*int64(h) + 2*claudeImageOutput + 6*(8*int64(w)+1) + 2<<20
	}
	// Keep the cost above capacity visible to the caller.
	return int(min(cost, int64(^uint(0)>>1)))
}

const claudeImageOutput = claudeImageBase64 / 4 * 3

// A failed encode cannot grow an output beyond the inline limit. Reuse its
// storage for the lower quality attempts; the cache gets only a sized copy.
type claudeImageWriter struct{ data []byte }

func (w *claudeImageWriter) Write(p []byte) (int, error) {
	if len(p) > cap(w.data)-len(w.data) {
		return 0, errors.New("image exceeds inline limit")
	}
	w.data = append(w.data, p...)
	return len(p), nil
}

func (w *claudeImageWriter) Bytes() []byte { return w.data }
func (w *claudeImageWriter) Reset()        { w.data = w.data[:0] }

// claudeImageLimits keys the images claudeImage made in fitted, apart
// from Codex's.
var claudeImageLimits = imageLimits{side: claudeImageSide, patches: -1}

const claudeImageOmitted = "[An image was left out: the gateway could not read it]"
const claudeImageOmittedBusy = "[An image was left out: the gateway was busy with other images; it will be tried again on the next turn]"
const claudeImageOmittedTIFF = "[An image was left out: TIFF images are not supported here; send it as PNG or JPEG]"
const claudeImageOmittedHEIC = "[An image was left out: this HEIC image could not be converted; send it as PNG or JPEG]"

// claudeImage is the real media type and base64 sent to Claude Code, or
// a note in their place. A cached verdict includes the declared type:
// an image a client labels differently must not borrow that verdict.
func claudeImage(mediaType, data string) (string, string, string) {
	e, _ := claudeImageVerdict(mediaType, data)
	if e.mime == "text/plain" {
		if len(e.data) != 0 {
			return "", "", string(e.data)
		}
		return "", "", claudeImageOmitted
	}
	if e.data == nil {
		return e.mime, data, ""
	}
	return e.mime, base64.StdEncoding.EncodeToString(e.data), ""
}

// Records describe the input bytes, including images replaced by notes.
// The verdict cache owns encoded output; this bounded companion keeps no
// image bytes and uses the very same key, including the full declared type.
type claudeImageRecord struct {
	declared, found, hash, action string
	bytes, width, height          int
}

var claudeImageRecords = struct {
	sync.Mutex
	m map[[32]byte]claudeImageRecord
}{m: make(map[[32]byte]claudeImageRecord)}

func claudeImageVerdict(declared, data string) (fitEntry, claudeImageRecord) {
	key := fitKey(strconv.Itoa(len(declared))+":"+declared+data, claudeImageLimits)
	claudeImageRecords.Lock()
	record, recorded := claudeImageRecords.m[key]
	claudeImageRecords.Unlock()
	e, ok := fitted.get(key)
	if !ok {
		var keep bool
		e, keep = claudeImageChecked(declared, data, &record)
		if keep {
			fitted.put(key, e)
		}
	} else if !recorded {
		// The verdict can outlive its record. Read only the header and
		// hash the bytes again, never decode the pixels a second time.
		record = claudeImageReadRecord(declared, data)
	}
	if !ok || !recorded {
		record.action = "as-is"
		if e.mime == "text/plain" {
			record.action = "note"
		} else if e.data != nil {
			record.action = "re-encoded"
		}
		claudeImageRecords.Lock()
		if len(claudeImageRecords.m) >= 8192 {
			for k := range claudeImageRecords.m {
				delete(claudeImageRecords.m, k)
				break
			}
		}
		claudeImageRecords.m[key] = record
		claudeImageRecords.Unlock()
	}
	return e, record
}

func claudeImageDetails(declared, data string) claudeImageRecord {
	key := fitKey(strconv.Itoa(len(declared))+":"+declared+data, claudeImageLimits)
	claudeImageRecords.Lock()
	record, ok := claudeImageRecords.m[key]
	claudeImageRecords.Unlock()
	if ok {
		return record
	}
	_, record = claudeImageVerdict(declared, data)
	return record
}

// An image over the read limit still gets a diagnostic without allocating
// its decoded bytes. This also fills a record evicted before its verdict.
func claudeImageReadRecord(declared, data string) claudeImageRecord {
	hash := sha256.New()
	count := &claudeImageCounter{}
	rd := bufio.NewReader(io.TeeReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)), io.MultiWriter(hash, count)))
	head, _ := rd.Peek(20)
	record := claudeImageRecord{declared: strings.Clone(declared[:min(len(declared), 64)]), found: claudeImageType(head)}
	if claudeImageTIFF(head) {
		record.found = "image/tiff"
	} else if isHEIC(head) {
		record.found = "image/heic"
	} else if (record.found != "" || bytes.HasPrefix(head, []byte("BM"))) && (record.found != "image/webp" || claudeImageWebPStart(head)) {
		if cfg, format, err := image.DecodeConfig(io.LimitReader(rd, 1<<20)); err == nil {
			record.width, record.height = cfg.Width, cfg.Height
			if record.found == "" {
				record.found = "image/" + format
			}
		}
	}
	_, _ = io.Copy(io.Discard, rd)
	record.bytes, record.hash = count.n, fmt.Sprintf("%x", hash.Sum(nil)[:4])
	return record
}

type claudeImageCounter struct{ n int }

func (c *claudeImageCounter) Write(b []byte) (int, error) {
	c.n += len(b)
	return len(b), nil
}

// TIFF is not decoded: its decoder's buffers follow tile and strip sizes
// the file chooses, rather than its image dimensions.
func claudeImageTIFF(raw []byte) bool {
	return bytes.HasPrefix(raw, []byte("II*\x00")) || bytes.HasPrefix(raw, []byte("MM\x00*"))
}

// claudeImageType reads only the magic number, never the client's label.
func claudeImageType(raw []byte) string {
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(raw, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(raw, []byte("GIF87a")), bytes.HasPrefix(raw, []byte("GIF89a")):
		return "image/gif"
	case len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// claudeImageComplete keeps a supported container Go cannot read (notably
// animated WebP) for Anthropic to judge, but never a cut-off container.
func claudeImageComplete(mt string, raw []byte) bool {
	switch mt {
	case "image/png":
		for i := 8; len(raw)-i >= 12; {
			n := uint64(binary.BigEndian.Uint32(raw[i:i+4])) + 12
			if n > uint64(len(raw)-i) {
				return false
			}
			if string(raw[i+4:i+8]) == "IEND" {
				return true
			}
			i += int(n)
		}
	case "image/jpeg":
		return claudeJPEGComplete(raw)
	case "image/gif":
		return claudeGIFComplete(raw)
	case "image/webp":
		n := uint64(binary.LittleEndian.Uint32(raw[4:8])) + 8
		return n >= 12 && n <= uint64(len(raw))
	}
	return false
}

// claudeGIFComplete walks every frame: image.Decode reads only the first.
// A trailer ends the image even when more bytes follow it.
func claudeGIFComplete(raw []byte) bool {
	if len(raw) < 13 {
		return false
	}
	i := 13
	if raw[10]&0x80 != 0 {
		i += 3 << (raw[10]&7 + 1)
	}
	for i < len(raw) {
		block := raw[i]
		i++
		switch block {
		case 0x3b:
			return true
		case 0x21:
			i++ // extension label
		case 0x2c:
			if len(raw)-i < 9 {
				return false
			}
			packed := raw[i+8]
			i += 9
			if packed&0x80 != 0 {
				i += 3 << (packed&7 + 1)
			}
			i++ // LZW code size
		default:
			return false
		}
		for {
			if i >= len(raw) {
				return false
			}
			n := int(raw[i])
			i++
			if n > len(raw)-i {
				return false
			}
			i += n
			if n == 0 {
				break
			}
		}
	}
	return false
}

// claudeJPEGComplete skips metadata segments: an EXIF thumbnail's EOI
// does not end the outer image. Stuffed bytes and restart markers inside
// a scan have no length; every other segment says how far to skip.
func claudeJPEGComplete(raw []byte) bool {
	scan := false
	for i := 2; i < len(raw); {
		if raw[i] != 0xff {
			if !scan {
				return false
			}
			i++
			continue
		}
		for i < len(raw) && raw[i] == 0xff {
			i++
		}
		if i == len(raw) {
			return false
		}
		marker := raw[i]
		i++
		switch {
		case marker == 0xd9:
			return true
		case marker == 0 || marker >= 0xd0 && marker <= 0xd7:
			if !scan {
				return false
			}
			continue
		}
		if i+2 > len(raw) {
			return false
		}
		n := int(binary.BigEndian.Uint16(raw[i : i+2]))
		if n < 2 || n > len(raw)-i {
			return false
		}
		i += n
		scan = marker == 0xda
	}
	return false
}

// webp/decode.go:decode skips unknown chunks (including ANIM/ANMF),
// reads ALPH before VP8, and returns at the first VP8/VP8L. Mirror that
// walk, including riff/Reader.Next's lengths and padding, without pixels.
// VP8L otherwise allocates from its own header before comparing the canvas.
type claudeImageWebP struct {
	width, height                  int
	lossless, alpha, alphaLossless bool
}

func claudeImageWebPStart(raw []byte) bool {
	if len(raw) < 20 {
		return false
	}
	switch string(raw[12:16]) {
	case "VP8 ", "VP8L", "VP8X":
		return true
	}
	return false
}

func claudeImageWebPFrame(raw []byte) (f claudeImageWebP, ok bool) {
	if !claudeImageWebPStart(raw) {
		return f, false
	}
	end := int(min(uint64(len(raw)), uint64(binary.LittleEndian.Uint32(raw[4:8]))+8))
	seenX, wantAlpha := false, false
	for i := 12; end-i >= 8; {
		kind := string(raw[i : i+4])
		n := uint64(binary.LittleEndian.Uint32(raw[i+4 : i+8]))
		i += 8
		if n > uint64(end-i) {
			return f, false
		}
		data := raw[i : i+int(n)]
		switch kind {
		case "VP8X":
			if seenX || n != 10 {
				return f, false
			}
			seenX, wantAlpha = true, data[0]&16 != 0
		case "ALPH":
			if !wantAlpha || n < 1 || data[0]&3 > 1 {
				return f, false
			}
			wantAlpha, f.alpha = false, true
			f.alphaLossless = data[0]&3 == 1
		case "VP8L":
			if f.alpha || len(data) < 5 || data[0] != 0x2f || data[4]>>5 != 0 {
				return f, false
			}
			bits := binary.LittleEndian.Uint32(data[1:5])
			f.width, f.height, f.lossless = int(bits&0x3fff)+1, int(bits>>14&0x3fff)+1, true
			return f, true
		case "VP8 ":
			if wantAlpha || n >= 1<<31 || len(data) < 10 || data[0]&1 != 0 || !bytes.Equal(data[3:6], []byte{0x9d, 1, 0x2a}) {
				return f, false
			}
			f.width = int(binary.LittleEndian.Uint16(data[6:8]) & 0x3fff)
			f.height = int(binary.LittleEndian.Uint16(data[8:10]) & 0x3fff)
			return f, true
		}
		n += n & 1
		if n > uint64(end-i) {
			return f, false
		}
		i += int(n)
	}
	return f, false
}

// vp8l/decode.go:decodeHuffmanGroups preallocates up to 65536 hGroups.
// huffman.go:hTree is a slice header plus 128 uint32 LUT entries, five
// per group. The test pins the compiled layout; 2680 also covers 32-bit.
const claudeImageHGroupBytes = 2680

func claudeImageLosslessHolds(w, h, input int64) int64 {
	// decodePix's NRGBA, inverseColorIndexing's second full image, and
	// decodeTransform/decodeHuffmanGroups' tile images (even a repeated
	// transform is decoded before rejection). 16*p covers all those pixels.
	// Each non-simple tree consumes at least 17 header bits before allocating
	// lengths; huffman.go:build/codeLengthsToCodes allocate at most 24*2328
	// bytes per tree. 32768/input byte covers these, simple nodes, rounding,
	// and decodePix's 8192-byte color caches. The fixed MiB covers partial
	// trees, subsidiary groups, palette transforms and reader state.
	return 16*w*h + 65536*claudeImageHGroupBytes + 32768*input + 1<<20
}

// Header-only bounds for the allocations made by the registered decoders.
// The fixed 128 KiB covers reader/decoder structs, palettes, LZW/zlib state
// (including flate's 32 KiB window), and per-allocation page rounding.
func claudeImageHolds(cfg image.Config, format string, raw []byte) (int, bool) {
	w, h := int64(cfg.Width), int64(cfg.Height)
	if w <= 0 || h <= 0 {
		return 0, false
	}
	if w > int64(holdsMost)/h {
		return holdsMost, true
	}
	p, cost := w*h, int64(128<<10)
	switch format {
	case "png":
		if len(raw) < 29 {
			return holdsMost, false
		}
		bpp := int64(4)
		if raw[24] == 16 {
			bpp = 8
		}
		if raw[25] == 3 {
			bpp = 1
		}
		// png/reader.go:readImagePass: tRNS may promote grey/truecolour
		// to NRGBA/NRGBA64 after DecodeConfig returned at IHDR.
		cost += bpp * p
		passes := int64(1)
		if raw[28] == 1 {
			// decode holds the final image beside pass images. Adam7
			// partitions the pixels, so all seven pass allocations sum to p.
			cost += bpp * p
			passes = 7
		}
		// readImagePass's current/previous filter rows, at most 8*w+1.
		cost += passes * 2 * (8*w + 1)
	case "jpeg":
		var ok bool
		cost, ok = claudeImageJPEGHolds(cfg, raw)
		if !ok {
			return holdsMost, false
		}
	case "gif":
		// gif/reader.go:newImageFromDescriptor checks frame <= canvas;
		// readImageDescriptor/uninterlace hold two paletted pixel arrays.
		cost += 2 * p
	case "bmp":
		// bmp/reader.go:decodePaletted has one byte/pixel and a padded
		// row. decodeRGB has RGBA plus a BGR row; decodeNRGBA has NRGBA.
		if _, paletted := cfg.ColorModel.(color.Palette); paletted {
			cost += p + w + 4
		} else {
			cost += 4*p + 3*w + 4
		}
	case "webp":
		f, ok := claudeImageWebPFrame(raw)
		if !ok || f.width != cfg.Width || f.height != cfg.Height {
			return holdsMost, false
		}
		if f.lossless {
			cost += claudeImageLosslessHolds(w, h, int64(len(raw)))
		} else {
			mx, my := (w+15)/16, (h+15)/16
			// vp8/decode.go:ensureImg: padded 4:2:0 planes, filterParam
			// per macroblock and mb per column. parseOtherHeaders and
			// parseOtherPartitions copy input; a malformed first partition
			// can ask for 19 bits' worth before its read fails.
			cost += 384*mx*my + 4*mx*my + 6*mx + int64(len(raw)) + 1<<19
			if f.alpha {
				cost += p
			} // webp/decode.go:readAlpha's A plane
			if f.alphaLossless {
				cost += claudeImageLosslessHolds(w, h, int64(len(raw)))
			}
		}
	default:
		return 0, false
	}
	return int(min(cost, int64(holdsMost))), true
}

// jpeg/reader.go:processSOF and isRGB decide sampling and colour. Metadata
// may follow SOF or scans, so skip entropy and inspect every marker through
// EOI, as the decoder does. No pixels are read.
func claudeImageJPEGHolds(cfg image.Config, raw []byte) (int64, bool) {
	var hv [4][2]int64
	var ids [4]byte
	nComp, progressive, jfif, adobeRGB := 0, false, false, false
	for i := 2; i < len(raw); {
		if raw[i] != 0xff {
			i++
			continue
		}
		for i < len(raw) && raw[i] == 0xff {
			i++
		}
		if i == len(raw) {
			break
		}
		marker := raw[i]
		i++
		if marker == 0xd9 {
			break
		}
		if marker == 0 || marker == 1 || marker >= 0xd0 && marker <= 0xd8 {
			continue
		}
		if len(raw)-i < 2 {
			return 0, false
		}
		n := int(binary.BigEndian.Uint16(raw[i : i+2]))
		if n < 2 || n > len(raw)-i {
			return 0, false
		}
		seg := raw[i+2 : i+n]
		switch marker {
		case 0xe0:
			if len(seg) >= 5 {
				jfif = string(seg[:5]) == "JFIF\x00"
			}
		case 0xee:
			if len(seg) >= 12 && string(seg[:5]) == "Adobe" {
				adobeRGB = seg[11] == 0
			}
		case 0xc0, 0xc1, 0xc2:
			if nComp != 0 || len(seg) < 6 || seg[0] != 8 {
				return 0, false
			}
			nComp, progressive = int(seg[5]), marker == 0xc2
			if (nComp != 1 && nComp != 3 && nComp != 4) || len(seg) != 6+3*nComp {
				return 0, false
			}
			for c := 0; c < nComp; c++ {
				ids[c] = seg[6+3*c]
				hv[c] = [2]int64{int64(seg[7+3*c] >> 4), int64(seg[7+3*c] & 15)}
				if hv[c][0] < 1 || hv[c][0] > 4 || hv[c][1] < 1 || hv[c][1] > 4 {
					return 0, false
				}
			}
			if nComp == 1 {
				hv[0] = [2]int64{1, 1}
			}
		}
		i += n
	}
	if nComp == 0 {
		return 0, false
	}
	w, h := int64(cfg.Width), int64(cfg.Height)
	mx, my := (w+8*hv[0][0]-1)/(8*hv[0][0]), (h+8*hv[0][1]-1)/(8*hv[0][1])
	cost := int64(128 << 10)
	var planes int64
	for c := 0; c < nComp; c++ {
		// jpeg/scan.go:makeImg's MCU-padded planes, blackPix included.
		plane := 64 * mx * my * hv[c][0] * hv[c][1]
		cost += plane
		// processSOS keeps 64 int32 coefficients per padded block.
		planes += plane
	}
	if progressive {
		cost += max(4*planes, 4*int64(nComp)*w*h)
	}
	if nComp == 4 || nComp == 3 && !jfif && (adobeRGB || ids == [4]byte{'R', 'G', 'B', 0}) {
		// jpeg/reader.go:convertToRGB/applyBlack's RGBA or CMYK copy.
		cost += 4 * w * h
	}
	return cost, true
}

func claudeImageChecked(declared, data string, record *claudeImageRecord) (fitEntry, bool) {
	declared = strings.Clone(declared[:min(len(declared), 64)])
	mt := ""
	*record = claudeImageRecord{declared: declared}
	omit := func(reason string, size int) fitEntry {
		log.Printf("claude: image replaced by a note (declared %q, found %q, %d bytes): %s; %dx%d sha256 %s", declared, mt, size, reason, record.width, record.height, record.hash)
		return fitEntry{mime: "text/plain"}
	}
	if len(data) > codexImageMaxBytes {
		*record = claudeImageReadRecord(declared, data)
		mt = record.found
		return omit("image exceeds the read limit", base64.StdEncoding.DecodedLen(len(data))), true
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	sum := sha256.Sum256(raw)
	record.found, record.bytes, record.hash = claudeImageType(raw), len(raw), fmt.Sprintf("%x", sum[:4])
	if claudeImageTIFF(raw) {
		record.found, mt = "image/tiff", "image/tiff"
		if err != nil {
			return omit("invalid base64", len(raw)), true
		}
		e := omit("TIFF is not converted", len(raw))
		e.data = []byte(claudeImageOmittedTIFF)
		return e, true
	}
	// heicAsJPEG converts a request's HEIC before this with its own bounds.
	// One it let through is not decoded here, where nothing bounds that decoder.
	if isHEIC(raw) {
		record.found, mt = "image/heic", "image/heic"
		if err != nil {
			return omit("invalid base64", len(raw)), true
		}
		e := omit("HEIC is not converted here", len(raw))
		e.data = []byte(claudeImageOmittedHEIC)
		return e, true
	}
	mt = record.found
	if mt == "" && !bytes.HasPrefix(raw, []byte("BM")) {
		return omit("unsupported image type", len(raw)), true
	}
	if mt == "image/webp" && !claudeImageWebPStart(raw) {
		return omit("unsupported WebP layout", len(raw)), true
	}
	cfg, format, headerErr := image.DecodeConfig(bytes.NewReader(raw))
	if headerErr == nil {
		record.width, record.height = cfg.Width, cfg.Height
	}
	mt = record.found
	if record.found == "" && headerErr == nil {
		record.found = "image/" + format
	}
	if err != nil {
		return omit("invalid base64", len(raw)), true
	}
	// GIF decoding stops at the first frame; WebP may promise more chunks.
	// PNG and JPEG decoding reads to the end of the image itself.
	if (mt == "image/gif" || mt == "image/webp") && !claudeImageComplete(mt, raw) {
		return omit("incomplete image container", len(raw)), true
	}
	if mt == "image/webp" {
		f, ok := claudeImageWebPFrame(raw)
		if !ok || headerErr == nil && (f.width != cfg.Width || f.height != cfg.Height) {
			return omit("WebP frame does not match its canvas", len(raw)), true
		}
	}
	if headerErr != nil {
		if claudeImageComplete(mt, raw) {
			return fitEntry{mime: mt}, true
		}
		return omit("unreadable image header", len(raw)), true
	}
	if mt == "" && format != "bmp" {
		return omit("unsupported image type", len(raw)), true
	}
	if format == "bmp" && cfg.Width > 0 && cfg.Height > 0 && (cfg.Width > claudeImageConvertPixels || cfg.Height > claudeImageConvertPixels/cfg.Width) {
		return omit("image too large to convert", len(raw)), true
	}
	refit := mt == "" || cfg.Width > claudeImageSide || cfg.Height > claudeImageSide || len(data) > claudeImageBase64
	// Complete GIF/WebP containers that fail Decode go unchanged anyway.
	// Without a refit there is no reason to decode their pixels or take a slot.
	if !refit && (format == "gif" || format == "webp") {
		return fitEntry{mime: mt}, true
	}
	holds, ok := claudeImageHolds(cfg, format, raw)
	weight := claudeImageDecodeWeight(cfg, holds, refit)
	if refit && (!ok || weight > codexImageMaxBytes) {
		return omit("image too large to convert", len(raw)), true
	}
	if !ok || holds > codexImageMaxBytes-len(raw) {
		if format == "jpeg" && ok {
			return omit("image exceeds the decode limit", len(raw)), true
		}
		if claudeImageComplete(mt, raw) {
			return fitEntry{mime: mt}, true
		}
		return omit("image exceeds the decode limit", len(raw)), true
	}
	if !claudeImageDecodeBudget.acquire(weight, claudeImageConvertWait) {
		e := omit("conversion slot busy", len(raw))
		e.data = []byte(claudeImageOmittedBusy)
		return e, false
	}
	defer claudeImageDecodeBudget.release(weight)
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		if claudeImageComplete(mt, raw) {
			return fitEntry{mime: mt}, true
		}
		return omit("unreadable image", len(raw)), true
	}
	if !refit {
		return fitEntry{mime: mt}, true
	}
	e := claudeImageRefit(src, cfg, format)
	if e.data == nil {
		return omit("image could not be re-encoded", len(raw)), true
	}
	reason := "image exceeds inline limits"
	if mt == "" {
		reason = "unsupported image type"
	}
	log.Printf("claude: image re-encoded as %s (declared %q, found %q, %d bytes): %s; %dx%d sha256 %s", e.mime, declared, "image/"+format, len(raw), reason, record.width, record.height, record.hash)
	return e, true
}

// claudeImageRefit scales a decoded image and writes it within the limits.
func claudeImageRefit(src image.Image, cfg image.Config, format string) fitEntry {
	var err error
	w, h := cfg.Width, cfg.Height
	if scale := math.Min(float64(claudeImageSide)/float64(max(w, h)), 1); scale < 1 {
		w = max(int(math.Round(float64(w)*scale)), 1)
		h = max(int(math.Round(float64(h)*scale)), 1)
		src = triangleResize(src, w, h)
	}
	fits := func(b []byte) bool { return base64.StdEncoding.EncodedLen(len(b)) <= claudeImageBase64 }
	out := claudeImageWriter{data: make([]byte, 0, claudeImageOutput)}
	if format == "jpeg" {
		err = jpeg.Encode(&out, src, &jpeg.Options{Quality: 85})
		if err == nil && fits(out.Bytes()) {
			return fitEntry{mime: "image/jpeg", data: bytes.Clone(out.Bytes())}
		}
	} else if err = png.Encode(&out, src); err == nil && fits(out.Bytes()) {
		return fitEntry{mime: "image/png", data: bytes.Clone(out.Bytes())}
	}
	// still over the limit: JPEG, on white where it was clear
	flat := image.NewRGBA(src.Bounds())
	draw.Draw(flat, flat.Rect, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Rect, src, src.Bounds().Min, draw.Over)
	for _, q := range []int{75, 60, 40} {
		out.Reset()
		if jpeg.Encode(&out, flat, &jpeg.Options{Quality: q}) == nil && fits(out.Bytes()) {
			return fitEntry{mime: "image/jpeg", data: bytes.Clone(out.Bytes())}
		}
	}
	return fitEntry{}
}
