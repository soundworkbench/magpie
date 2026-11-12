package gateway

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

func claudeImageGray(t *testing.T, format string, width, height int) []byte {
	t.Helper()
	m := image.NewGray(image.Rect(0, 0, width, height))
	var b bytes.Buffer
	var err error
	if format == "tiff" {
		err = tiff.Encode(&b, m, &tiff.Options{Compression: tiff.Deflate})
	} else {
		err = bmp.Encode(&b, m)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestClaudeImageTIFFIsNotConverted(t *testing.T) {
	valid := claudeImageGray(t, "tiff", 3, 2)
	for _, tc := range []struct {
		name, declared string
		raw            []byte
	}{
		{"valid", "image/tiff", valid},
		{"declared PNG", "image/png", valid},
		{"little endian garbage", "image/tiff", append([]byte("II*\x00"), bytes.Repeat([]byte{0xa5}, 64)...)},
		{"big endian garbage", "image/tiff", append([]byte("MM\x00*"), bytes.Repeat([]byte{0xa5}, 64)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := keepLog(t)
			encoded := base64.StdEncoding.EncodeToString(tc.raw)
			key := fitKey(fmt.Sprintf("%d:%s%s", len(tc.declared), tc.declared, encoded), claudeImageLimits)
			t.Cleanup(func() {
				fitted.Lock()
				defer fitted.Unlock()
				if el := fitted.m[key]; el != nil {
					fitted.drop(el)
				}
			})
			mt, data, note := claudeImage(tc.declared, encoded)
			record := claudeImageDetails(tc.declared, encoded)
			t.Logf("type %q, image %t, note %q; record %+v; %s", mt, data != "", note, record, strings.TrimSpace(logs.String()))
			if mt != "" || data != "" || note != claudeImageOmittedTIFF {
				t.Error("TIFF was not replaced by the note")
			}
			if mt, data, note := claudeImage(tc.declared, encoded); mt != "" || data != "" || note != claudeImageOmittedTIFF {
				t.Error("cached TIFF verdict lost its note")
			}
			if !strings.Contains(logs.String(), "TIFF is not converted") {
				t.Error("missing TIFF refusal reason")
			}
			sum := sha256.Sum256(tc.raw)
			if record.found != "image/tiff" || record.action != "note" || record.bytes != len(tc.raw) || record.hash != fmt.Sprintf("%x", sum[:4]) || record.width != 0 || record.height != 0 {
				t.Error("TIFF record lost its type, action, bytes or hash, or read its dimensions")
			}
		})
	}
	t.Run("invalid base64", func(t *testing.T) {
		logs := keepLog(t)
		encoded := base64.StdEncoding.EncodeToString(valid) + "%"
		key := fitKey("10:image/tiff"+encoded, claudeImageLimits)
		t.Cleanup(func() {
			fitted.Lock()
			defer fitted.Unlock()
			if el := fitted.m[key]; el != nil {
				fitted.drop(el)
			}
		})
		for range 2 {
			mt, data, note := claudeImage("image/tiff", encoded)
			if mt != "" || data != "" || note != claudeImageOmitted {
				t.Fatalf("invalid base64: type %q, image %t, note %q", mt, data != "", note)
			}
		}
		t.Log(strings.TrimSpace(logs.String()))
		if !strings.Contains(logs.String(), "invalid base64") {
			t.Error("missing invalid base64 reason")
		}
	})
}

func TestClaudeImageHEICIsNotDecoded(t *testing.T) {
	raw, err := os.ReadFile("testdata/opaque.heic")
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || format != "heic" {
		t.Fatalf("registered decoder: format %q, config %+v, error %v", format, cfg, err)
	}
	t.Logf("registered decoder: %s, %dx%d", format, cfg.Width, cfg.Height)
	const wantNote = "[An image was left out: this HEIC image could not be converted; send it as PNG or JPEG]"
	for _, declared := range []string{"image/heic", "image/png"} {
		t.Run(declared, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(raw)
			key := fitKey(fmt.Sprintf("%d:%s%s", len(declared), declared, encoded), claudeImageLimits)
			t.Cleanup(func() {
				fitted.Lock()
				defer fitted.Unlock()
				if el := fitted.m[key]; el != nil {
					fitted.drop(el)
				}
			})
			logs := keepLog(t)
			mt, data, note := claudeImage(declared, encoded)
			record := claudeImageDetails(declared, encoded)
			t.Logf("type %q, image %t, note %q; record %+v; %s", mt, data != "", note, record, strings.TrimSpace(logs.String()))
			if mt != "" || data != "" || note != wantNote {
				t.Error("HEIC was not replaced by the note")
			}
			if mt, data, note := claudeImage(declared, encoded); mt != "" || data != "" || note != wantNote {
				t.Error("cached HEIC verdict lost its note")
			}
			if !strings.Contains(logs.String(), "HEIC is not converted here") {
				t.Error("missing HEIC refusal reason")
			}
			sum := sha256.Sum256(raw)
			if record.found != "image/heic" || record.action != "note" || record.bytes != len(raw) || record.hash != fmt.Sprintf("%x", sum[:4]) || record.width != 0 || record.height != 0 {
				t.Error("HEIC record lost its type, action, bytes or hash, or read its dimensions")
			}
			record = claudeImageReadRecord(declared, encoded)
			t.Logf("read record: %+v", record)
			if record.found != "image/heic" || record.bytes != len(raw) || record.hash != fmt.Sprintf("%x", sum[:4]) || record.width != 0 || record.height != 0 {
				t.Error("HEIC read record lost its type, bytes or hash, or read its dimensions")
			}
		})
	}
}

var (
	claudeImageTestFormatOnce sync.Once
	claudeImageTestDecodes    atomic.Int32
)

func TestClaudeImageOtherRegisteredFormatIsNotDecoded(t *testing.T) {
	claudeImageTestFormatOnce.Do(func() {
		image.RegisterFormat("claudeimagetest", "CLIMGTEST", func(io.Reader) (image.Image, error) {
			claudeImageTestDecodes.Add(1)
			return image.NewRGBA(image.Rect(0, 0, 10, 10)), nil
		}, func(io.Reader) (image.Config, error) {
			return image.Config{ColorModel: color.RGBAModel, Width: 10, Height: 10}, nil
		})
	})
	claudeImageTestDecodes.Store(0)
	raw := []byte("CLIMGTESTabcd")
	encoded := base64.StdEncoding.EncodeToString(raw)
	key := fitKey("9:image/png"+encoded, claudeImageLimits)
	t.Cleanup(func() {
		fitted.Lock()
		defer fitted.Unlock()
		if el := fitted.m[key]; el != nil {
			fitted.drop(el)
		}
	})
	logs := keepLog(t)
	mt, data, note := claudeImage("image/png", encoded)
	record := claudeImageDetails("image/png", encoded)
	t.Logf("type %q, image %t, note %q; record %+v; decode calls %d; %s", mt, data != "", note, record, claudeImageTestDecodes.Load(), strings.TrimSpace(logs.String()))
	if mt != "" || data != "" || note != claudeImageOmitted {
		t.Error("registered format was not replaced by the note")
	}
	if !strings.Contains(logs.String(), "unsupported image type") {
		t.Error("missing unsupported image type reason")
	}
	if record.found != "image/claudeimagetest" || record.width != 10 || record.height != 10 || record.action != "note" {
		t.Error("registered format record lost its type, dimensions or action")
	}
	if calls := claudeImageTestDecodes.Load(); calls != 0 {
		t.Errorf("registered format decoded %d times, want 0", calls)
	}
}

func TestClaudeImageConversionSlotWaitIsBounded(t *testing.T) {
	// BMP conversion tests are sequential. Keep the wait unchanged during each call.
	oldWait := claudeImageConvertWait
	claudeImageConvertWait = 200 * time.Millisecond
	t.Cleanup(func() { claudeImageConvertWait = oldWait })
	claudeImageConvertSlot <- struct{}{}
	held := true
	t.Cleanup(func() {
		if held {
			<-claudeImageConvertSlot
		}
	})
	raw := claudeImageGray(t, "bmp", 7, 3)
	encoded := base64.StdEncoding.EncodeToString(raw)
	key := fitKey("9:image/bmp"+encoded, claudeImageLimits)
	t.Cleanup(func() {
		fitted.Lock()
		defer fitted.Unlock()
		if el := fitted.m[key]; el != nil {
			fitted.drop(el)
		}
	})
	logs := keepLog(t)
	start := time.Now()
	mt, data, note := claudeImage("image/bmp", encoded)
	elapsed := time.Since(start)
	t.Logf("occupied slot: %s, type %q, image %t, note %q; %s", elapsed, mt, data != "", note, strings.TrimSpace(logs.String()))
	if elapsed >= time.Second || mt != "" || data != "" || note != claudeImageOmitted {
		t.Fatal("occupied conversion slot did not return the note within a second")
	}
	if !strings.Contains(logs.String(), "conversion slot busy") {
		t.Error("missing conversion slot reason")
	}
	start = time.Now()
	record := claudeImageDetails("image/bmp", encoded)
	elapsed = time.Since(start)
	t.Logf("details: %s, record %+v", elapsed, record)
	if elapsed >= claudeImageConvertWait {
		t.Error("details waited for the conversion slot again")
	}
	sum := sha256.Sum256(raw)
	if record.action != "note" || record.found != "image/bmp" || record.bytes != len(raw) || record.hash != fmt.Sprintf("%x", sum[:4]) {
		t.Error("busy conversion record lost its action, type, bytes or hash")
	}
	<-claudeImageConvertSlot
	held = false
	mt, data, note = claudeImage("image/bmp", encoded)
	t.Logf("free slot: type %q, image %t, note %q", mt, data != "", note)
	if mt != "image/png" || data == "" || note != "" {
		t.Fatal("given-up verdict was kept instead of trying the conversion again")
	}
}

func TestClaudeImageTiledTIFFIsNotDecoded(t *testing.T) {
	var tile bytes.Buffer
	zw := zlib.NewWriter(&tile)
	zeros := make([]byte, 4096)
	for left := 17 * 2073616; left > 0; {
		n := min(left, len(zeros))
		if _, err := zw.Write(zeros[:n]); err != nil {
			t.Fatal(err)
		}
		left -= n
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte("II*\x00\x00\x00\x00\x00"), tile.Bytes()...)
	if len(raw)%2 != 0 {
		raw = append(raw, 0)
	}
	binary.LittleEndian.PutUint32(raw[4:8], uint32(len(raw)))
	tags := []struct {
		tag, kind uint16
		value     uint32
	}{
		{256, 4, 1},
		{257, 4, 2073600},
		{258, 3, 8},
		{259, 3, 8},
		{262, 3, 1},
		{277, 3, 1},
		{322, 4, 17},
		{323, 4, 2073616},
		{324, 4, 8},
		{325, 4, uint32(tile.Len())},
	}
	raw = binary.LittleEndian.AppendUint16(raw, uint16(len(tags)))
	for _, tag := range tags {
		raw = binary.LittleEndian.AppendUint16(raw, tag.tag)
		raw = binary.LittleEndian.AppendUint16(raw, tag.kind)
		raw = binary.LittleEndian.AppendUint32(raw, 1)
		raw = binary.LittleEndian.AppendUint32(raw, tag.value)
	}
	raw = binary.LittleEndian.AppendUint32(raw, 0)
	// Only read the header: decoding this tile would allocate about 900 MiB.
	cfg, err := tiff.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width != 1 || cfg.Height != 2073600 {
		t.Fatalf("fixture header: %+v, %v", cfg, err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	key := fitKey("9:image/png"+encoded, claudeImageLimits)
	t.Cleanup(func() {
		fitted.Lock()
		defer fitted.Unlock()
		if el := fitted.m[key]; el != nil {
			fitted.drop(el)
		}
	})
	logs := keepLog(t)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	mt, data, note := claudeImage("image/png", encoded)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("fixture %dx%d, %d bytes; allocated %d bytes; type %q, image %t, note %q; %s", cfg.Width, cfg.Height, len(raw), allocated, mt, data != "", note, strings.TrimSpace(logs.String()))
	if mt != "" || data != "" || note != claudeImageOmittedTIFF {
		t.Error("tiled TIFF was not replaced by the TIFF note")
	}
	if !strings.Contains(logs.String(), "TIFF is not converted") {
		t.Error("missing TIFF refusal reason")
	}
	if allocated >= 8<<20 {
		t.Errorf("TIFF refusal allocated %d bytes, want less than 8 MiB", allocated)
	}
}

func TestClaudeImageConversionPixelLimit(t *testing.T) {
	for _, format := range []string{"bmp"} {
		for _, size := range []struct {
			width, height int
			omitted       bool
		}{{4000, 4000, true}, {3840, 2160, false}} {
			t.Run(fmt.Sprintf("%s/%dx%d", format, size.width, size.height), func(t *testing.T) {
				logs := keepLog(t)
				raw := claudeImageGray(t, format, size.width, size.height)
				encoded := base64.StdEncoding.EncodeToString(raw)
				key := fitKey("9:image/png"+encoded, claudeImageLimits)
				t.Cleanup(func() {
					fitted.Lock()
					defer fitted.Unlock()
					if el := fitted.m[key]; el != nil {
						fitted.drop(el)
					}
				})
				// A supported declared type must not bypass the conversion limit.
				mt, data, note := claudeImage("image/png", encoded)
				t.Logf("type %q, image %t, note %q; %s", mt, data != "", note, strings.TrimSpace(logs.String()))
				if size.omitted {
					if mt != "" || data != "" || note != claudeImageOmitted {
						t.Errorf("oversized %s was not replaced by the note", format)
					}
					if !strings.Contains(logs.String(), "image too large to convert") {
						t.Error("missing conversion limit reason")
					}
				} else if mt != "image/png" || data == "" || note != "" {
					t.Fatal("image at the conversion limit was not converted to PNG")
				}
			})
		}
	}
}

func TestClaudeImageConversionHugeSides(t *testing.T) {
	for _, size := range []struct{ width, height uint32 }{
		// The largest square whose width*height*4 fits a 64-bit int.
		{1518500249, 1518500249},
		{1, 8294401},
		{8294401, 1},
	} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			raw := claudeImageGray(t, "bmp", 1, 1)
			binary.LittleEndian.PutUint32(raw[18:], size.width)
			binary.LittleEndian.PutUint32(raw[22:], size.height)
			cfg, err := bmp.DecodeConfig(bytes.NewReader(raw))
			if err != nil || cfg.Width != int(size.width) || cfg.Height != int(size.height) {
				t.Fatalf("fixture header: %+v, %v", cfg, err)
			}
			logs := keepLog(t)
			encoded := base64.StdEncoding.EncodeToString(raw)
			key := fitKey("9:image/bmp"+encoded, claudeImageLimits)
			t.Cleanup(func() {
				fitted.Lock()
				defer fitted.Unlock()
				if el := fitted.m[key]; el != nil {
					fitted.drop(el)
				}
			})
			mt, data, note := claudeImage("image/bmp", encoded)
			t.Logf("type %q, image %t, note %q; %s", mt, data != "", note, strings.TrimSpace(logs.String()))
			if mt != "" || data != "" || note != claudeImageOmitted {
				t.Error("huge sides were not replaced by the note")
			}
			if !strings.Contains(logs.String(), "image too large to convert") {
				t.Error("missing conversion limit reason")
			}
		})
	}
}

func TestClaudeImageRecordLimitsTIFFHeader(t *testing.T) {
	raw := claudeImageGray(t, "tiff", 1, 1)
	ifd := int(binary.LittleEndian.Uint32(raw[4:8]))
	const offset = (1 << 20) + 16
	// Keep the pixels in place and move the directory beyond the header budget.
	long := make([]byte, offset+len(raw)-ifd)
	copy(long, raw)
	copy(long[offset:], raw[ifd:])
	binary.LittleEndian.PutUint32(long[4:], offset)
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(long)); err != nil || cfg.Width != 1 || cfg.Height != 1 {
		t.Fatalf("fixture header: %+v, %v", cfg, err)
	}
	record := claudeImageReadRecord("image/tiff", base64.StdEncoding.EncodeToString(long))
	t.Logf("record: %+v", record)
	sum := sha256.Sum256(long)
	if record.found != "image/tiff" || record.bytes != len(long) || record.hash != fmt.Sprintf("%x", sum[:4]) {
		t.Fatal("TIFF record lost its type, bytes or hash")
	}
	if record.width != 0 || record.height != 0 {
		t.Fatal("header beyond 1 MiB supplied dimensions")
	}
}

func TestClaudeImageRecordLimitsHeaderRead(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 3, 2)), nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name              string
		segments, payload int
		width, height     int
	}{
		{"beyond limit", 17, 65533, 0, 0},
		{"within limit", 1, 16, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := append([]byte(nil), b.Bytes()[:2]...)
			segment := make([]byte, 4+tc.payload)
			segment[0], segment[1] = 0xff, 0xe1 // APP1, after SOI and before the frame header.
			binary.BigEndian.PutUint16(segment[2:], uint16(tc.payload+2))
			for range tc.segments {
				raw = append(raw, segment...)
			}
			raw = append(raw, b.Bytes()[2:]...)
			if cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw)); err != nil || cfg.Width != 3 || cfg.Height != 2 {
				t.Fatalf("fixture header: %+v, %v", cfg, err)
			}
			record := claudeImageReadRecord("image/jpeg", base64.StdEncoding.EncodeToString(raw))
			t.Logf("record: %+v", record)
			sum := sha256.Sum256(raw)
			if record.found != "image/jpeg" || record.bytes != len(raw) || record.hash != fmt.Sprintf("%x", sum[:4]) {
				t.Error("header limit lost the type, bytes or hash")
			}
			if record.width != tc.width || record.height != tc.height {
				t.Errorf("dimensions %dx%d, want %dx%d", record.width, record.height, tc.width, tc.height)
			}
		})
	}
}
