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

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
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
// for byte. An unsupported readable image goes as PNG, an incomplete or
// unreadable one as a note. Complete supported containers Go cannot decode
// go to Anthropic unchanged, with their real type.
const (
	claudeImageSide   = 2000
	claudeImageBase64 = 5 << 20
)

// claudeImageLimits keys the images claudeImage made in fitted, apart
// from Codex's.
var claudeImageLimits = imageLimits{side: claudeImageSide, patches: -1}

const claudeImageOmitted = "[An image was left out: the gateway could not read it]"

// claudeImage is the real media type and base64 sent to Claude Code, or
// a note in their place. A cached verdict includes the declared type:
// an image a client labels differently must not borrow that verdict.
func claudeImage(mediaType, data string) (string, string, string) {
	e, _ := claudeImageVerdict(mediaType, data)
	if e.mime == "text/plain" {
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
		e = claudeImageChecked(declared, data, &record)
		fitted.put(key, e)
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
	_, record := claudeImageVerdict(declared, data)
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
	if cfg, format, err := image.DecodeConfig(rd); err == nil {
		record.width, record.height = cfg.Width, cfg.Height
		if record.found == "" {
			record.found = "image/" + format
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

func claudeImageChecked(declared, data string, record *claudeImageRecord) fitEntry {
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
		return omit("image exceeds the read limit", base64.StdEncoding.DecodedLen(len(data)))
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	sum := sha256.Sum256(raw)
	cfg, format, headerErr := image.DecodeConfig(bytes.NewReader(raw))
	record.found, record.bytes, record.hash = claudeImageType(raw), len(raw), fmt.Sprintf("%x", sum[:4])
	if headerErr == nil {
		record.width, record.height = cfg.Width, cfg.Height
	}
	mt = record.found
	if record.found == "" && headerErr == nil {
		record.found = "image/" + format
	}
	if err != nil {
		return omit("invalid base64", len(raw))
	}
	// GIF decoding stops at the first frame; WebP may promise more chunks.
	// PNG and JPEG decoding reads to the end of the image itself.
	if (mt == "image/gif" || mt == "image/webp") && !claudeImageComplete(mt, raw) {
		return omit("incomplete image container", len(raw))
	}
	if headerErr != nil {
		if claudeImageComplete(mt, raw) {
			return fitEntry{mime: mt}
		}
		return omit("unreadable image header", len(raw))
	}
	holds, ok := imageHolds(cfg, format)
	if !ok || holds > codexImageMaxBytes-len(raw) {
		if claudeImageComplete(mt, raw) {
			return fitEntry{mime: mt}
		}
		return omit("image exceeds the decode limit", len(raw))
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		if claudeImageComplete(mt, raw) {
			return fitEntry{mime: mt}
		}
		return omit("unreadable image", len(raw))
	}
	if mt != "" && cfg.Width <= claudeImageSide && cfg.Height <= claudeImageSide && len(data) <= claudeImageBase64 {
		return fitEntry{mime: mt}
	}
	e := claudeImageRefit(src, cfg, format)
	if e.data == nil {
		return omit("image could not be re-encoded", len(raw))
	}
	reason := "image exceeds inline limits"
	if mt == "" {
		reason = "unsupported image type"
	}
	log.Printf("claude: image re-encoded as %s (declared %q, found %q, %d bytes): %s; %dx%d sha256 %s", e.mime, declared, "image/"+format, len(raw), reason, record.width, record.height, record.hash)
	return e
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
