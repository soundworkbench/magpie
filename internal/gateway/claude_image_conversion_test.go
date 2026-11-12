package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"strings"
	"testing"

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
			mt, data, note := claudeImage(tc.declared, encoded)
			record := claudeImageDetails(tc.declared, encoded)
			t.Logf("type %q, image %t, note %q; record %+v; %s", mt, data != "", note, record, strings.TrimSpace(logs.String()))
			if mt != "" || data != "" || note != claudeImageOmitted {
				t.Error("TIFF was not replaced by the note")
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
				// A supported declared type must not bypass the conversion limit.
				mt, data, note := claudeImage("image/png", base64.StdEncoding.EncodeToString(raw))
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
			mt, data, note := claudeImage("image/bmp", base64.StdEncoding.EncodeToString(raw))
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
