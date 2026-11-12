package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
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

func TestClaudeImageConversionPixelLimit(t *testing.T) {
	for _, format := range []string{"tiff", "bmp"} {
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
	raw := claudeImageGray(t, "tiff", 1, 1)
	ifd := int(binary.LittleEndian.Uint32(raw[4:8]))
	entries := int(binary.LittleEndian.Uint16(raw[ifd:]))
	for i := 0; i < entries; i++ {
		entry := raw[ifd+2+i*12:][:12]
		tag := binary.LittleEndian.Uint16(entry)
		if tag == 256 || tag == 257 { // ImageWidth and ImageLength, as LONGs.
			binary.LittleEndian.PutUint16(entry[2:], 4)
			binary.LittleEndian.PutUint32(entry[8:], 0xfffffff0)
		}
	}
	logs := keepLog(t)
	mt, data, note := claudeImage("image/tiff", base64.StdEncoding.EncodeToString(raw))
	t.Logf("type %q, image %t, note %q; %s", mt, data != "", note, strings.TrimSpace(logs.String()))
	if mt != "" || data != "" || note != claudeImageOmitted {
		t.Fatal("huge sides were not replaced by the note")
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
	if record.bytes != len(long) || record.hash != fmt.Sprintf("%x", sum[:4]) {
		t.Fatal("header limit lost bytes from the count or hash")
	}
	if record.width != 0 || record.height != 0 {
		t.Fatal("header beyond 1 MiB supplied dimensions")
	}
}
