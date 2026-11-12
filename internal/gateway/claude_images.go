package gateway

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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
// type, unless a WebP frame mismatches its canvas or progressive JPEG
// coefficients exceed the decode limit. A busy decode goes as a note and
// is tried again on the next turn.
const (
	claudeImageSide   = 2000
	claudeImageBase64 = 5 << 20
)

// BMP keeps its pixel limit even when its decode, refit and encode fit
// the shared byte budget. The budget does not count the source bytes.
const claudeImageConvertPixels = 3840 * 2160

// All decodes share this budget through refit and encoding. It bounds
// their combined holds, excluding raw, except that an image whose cost
// exceeds the capacity takes it all and runs alone. A bounded wait lets
// a busy request return a note and try the image again next turn.
var (
	claudeImageDecodeBudget claudeImageByteBudget
	claudeImageConvertWait  = 20 * time.Second
)

type claudeImageByteBudget struct {
	mu      sync.Mutex
	used    int
	changed chan struct{}
}

func (b *claudeImageByteBudget) acquire(n int, wait time.Duration) bool {
	if n <= 0 || n > codexImageMaxBytes {
		return false
	}
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

// claudeImageDecodeWeight counts the source and what the refit can keep
// beside it. triangleResize copies 4*sw*sh RGBA bytes and holds 16*w*sh
// float32 bytes between passes. Its two sets of triangleWeights have at
// most 2*src+2*n floats per axis; allow doubled slice capacity, taps and
// allocation rounding with 16*(sw+sh)+96*(w+h). The output allowance is
// 8 bytes a pixel for resized/flattened copies plus 72 for encoded bytes
// and bytes.Buffer growth (at most 24 per pixel, including JPEG stuffing,
// rounded to 16x16 JPEG MCU bounds). Another 2 MiB covers encoder state and rows.
func claudeImageDecodeWeight(cfg image.Config, holds int, refit bool) int {
	cost := int64(holds)
	if refit {
		sw, sh := int64(cfg.Width), int64(cfg.Height)
		w, h := cfg.Width, cfg.Height
		if scale := math.Min(float64(claudeImageSide)/float64(max(w, h)), 1); scale < 1 {
			w = max(int(math.Round(float64(w)*scale)), 1)
			h = max(int(math.Round(float64(h)*scale)), 1)
			cost += 4*sw*sh + 16*int64(w)*sh + 16*(sw+sh) + 96*int64(w+h)
		}
		cost += 80*int64((w+15)/16*16)*int64((h+15)/16*16) + 2<<20
	}
	return int(min(cost, int64(codexImageMaxBytes)))
}

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
	head, _ := rd.Peek(12)
	record := claudeImageRecord{declared: strings.Clone(declared[:min(len(declared), 64)]), found: claudeImageType(head)}
	if claudeImageTIFF(head) {
		record.found = "image/tiff"
	} else if isHEIC(head) {
		record.found = "image/heic"
	} else if record.found != "" || bytes.HasPrefix(head, []byte("BM")) {
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

// VP8X gives DecodeConfig the canvas, but VP8L allocates from its own
// frame header before webp compares the two. Read that header first.
func claudeImageWebPFrame(raw []byte) (width, height int, ok bool) {
	if claudeImageType(raw) != "image/webp" || len(raw) < 20 || string(raw[12:16]) != "VP8X" {
		return 0, 0, false
	}
	end := int(min(uint64(len(raw)), uint64(binary.LittleEndian.Uint32(raw[4:8]))+8))
	for i := 12; end-i >= 8; {
		kind := string(raw[i : i+4])
		n := uint64(binary.LittleEndian.Uint32(raw[i+4 : i+8]))
		i += 8
		data := raw[i : i+int(min(n, uint64(end-i)))]
		switch kind {
		case "VP8L":
			if len(data) >= 5 && data[0] == 0x2f {
				bits := binary.LittleEndian.Uint32(data[1:5])
				return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, true
			}
			return 0, 0, false
		case "VP8 ":
			if len(data) >= 10 && bytes.Equal(data[3:6], []byte{0x9d, 0x01, 0x2a}) {
				return int(binary.LittleEndian.Uint16(data[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(data[8:10]) & 0x3fff), true
			}
			return 0, 0, false
		}
		n += n & 1
		if n > uint64(end-i) {
			break
		}
		i += int(n)
	}
	return 0, 0, false
}

// Progressive JPEG keeps coefficient blocks for every component until
// the last scan, besides the pixels counted by imageHolds.
func claudeImageHolds(cfg image.Config, format string, raw []byte) (int, bool) {
	holds, ok := imageHolds(cfg, format)
	if !ok || holds == holdsMost || format != "jpeg" || !bytes.HasPrefix(raw, []byte{0xff, 0xd8}) {
		return holds, ok
	}
	for i := 2; i < len(raw); {
		if raw[i] != 0xff {
			// image/jpeg tolerates stray bytes between markers.
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
		if marker == 0xda || marker == 0xd9 {
			break
		}
		if marker == 0 || marker == 0x01 || marker >= 0xd0 && marker <= 0xd8 {
			continue
		}
		if len(raw)-i < 2 {
			break
		}
		n := int(binary.BigEndian.Uint16(raw[i : i+2]))
		if n < 2 || n > len(raw)-i {
			break
		}
		if marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc {
			if marker == 0xc2 && n >= 8 {
				cost := int64(holds) + 4*int64(raw[i+7])*int64(cfg.Width)*int64(cfg.Height)
				return int(min(cost, int64(holdsMost))), true
			}
			break
		}
		i += n
	}
	return holds, ok
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
	if w, h, ok := claudeImageWebPFrame(raw); ok && (w != cfg.Width || h != cfg.Height) {
		return omit("WebP frame does not match its canvas", len(raw)), true
	}
	holds, ok := claudeImageHolds(cfg, format, raw)
	if !ok || holds > codexImageMaxBytes-len(raw) {
		// A progressive frame whose coefficients exceed the limit must
		// not borrow the complete-container fallback for unreadable files.
		if pixels, _ := imageHolds(cfg, format); format == "jpeg" && holds > pixels {
			return omit("image exceeds the decode limit", len(raw)), true
		}
		if claudeImageComplete(mt, raw) {
			return fitEntry{mime: mt}, true
		}
		return omit("image exceeds the decode limit", len(raw)), true
	}
	refit := mt == "" || cfg.Width > claudeImageSide || cfg.Height > claudeImageSide || len(data) > claudeImageBase64
	weight := claudeImageDecodeWeight(cfg, holds, refit)
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
	var out bytes.Buffer
	if format == "jpeg" {
		err = jpeg.Encode(&out, src, &jpeg.Options{Quality: 85})
		if err == nil && fits(out.Bytes()) {
			return fitEntry{mime: "image/jpeg", data: out.Bytes()}
		}
	} else if err = png.Encode(&out, src); err == nil && fits(out.Bytes()) {
		return fitEntry{mime: "image/png", data: out.Bytes()}
	}
	// still over the limit: JPEG, on white where it was clear
	flat := image.NewRGBA(src.Bounds())
	draw.Draw(flat, flat.Rect, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Rect, src, src.Bounds().Min, draw.Over)
	for _, q := range []int{75, 60, 40} {
		out.Reset()
		if jpeg.Encode(&out, flat, &jpeg.Options{Quality: q}) == nil && fits(out.Bytes()) {
			return fitEntry{mime: "image/jpeg", data: out.Bytes()}
		}
	}
	return fitEntry{}
}
