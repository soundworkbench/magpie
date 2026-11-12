package gateway

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"runtime"
	"testing"
)

func codexJPEGSegment(marker byte, payload []byte) []byte {
	n := len(payload) + 2
	return append([]byte{0xff, marker, byte(n >> 8), byte(n)}, payload...)
}

func codexJPEGHeader(marker byte, w, h int, components byte, sampling ...byte) []byte {
	raw := []byte{0xff, 0xd8}
	raw = append(raw, codexJPEGSegment(0xe0, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00"))...)
	frame := []byte{8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), components}
	for i := byte(1); i <= components; i++ {
		hv := byte(0x11)
		if len(sampling) > 0 {
			hv = sampling[i-1]
		}
		frame = append(frame, i, hv, 0)
	}
	return append(raw, codexJPEGSegment(marker, frame)...)
}

func TestFitHoldsProgressiveJPEG(t *testing.T) {
	for _, c := range []struct {
		name       string
		marker     byte
		w, h       int
		components byte
		want       int
	}{
		{"large progressive", 0xc2, 8000, 10000, 3, holdsMost},
		{"large baseline", 0xc0, 8000, 10000, 3, 320000000},
		{"small progressive", 0xc2, 32, 24, 3, 12288},
		{"small baseline", 0xc0, 32, 24, 3, 4 * 32 * 24},
		{"grey progressive", 0xc2, 32, 24, 1, 3840},
		{"grey baseline", 0xc0, 32, 24, 1, 32 * 24},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := codexJPEGHeader(c.marker, c.w, c.h, c.components)
			b64 := base64.StdEncoding.EncodeToString(raw)
			want := min(c.want+base64.StdEncoding.DecodedLen(len(b64)), holdsMost)
			if got := fitHolds("data:image/jpeg;base64," + b64); got != want {
				t.Fatalf("hold = %d, want %d", got, want)
			}
		})
	}
}

func TestFitHoldsProgressiveSampling(t *testing.T) {
	for _, c := range []struct {
		name     string
		marker   byte
		w, h     int
		sampling []byte
		want     int
		refused  bool
	}{
		{"photo 420", 0xc2, 6000, 4000, []byte{0x22, 0x11, 0x11}, 240000000, false},
		{"photo 444", 0xc2, 6000, 4000, []byte{0x11, 0x11, 0x11}, holdsMost, true},
		{"large 420", 0xc2, 8000, 10000, []byte{0x22, 0x11, 0x11}, holdsMost, true},
		{"large 444", 0xc2, 8000, 10000, []byte{0x11, 0x11, 0x11}, holdsMost, true},
		{"padded 420", 0xc2, 32, 24, []byte{0x22, 0x11, 0x11}, 9216, false},
		{"grey nominal 42", 0xc2, 20, 5, []byte{0x42}, 868, false},
		{"baseline 420", 0xc0, 6000, 4000, []byte{0x22, 0x11, 0x11}, 96000000, false},
		{"largest header area 420", 0xc2, 512, 65535, []byte{0x22, 0x11, 0x11}, 335542272, false},
		{"square 420 fits", 0xc2, 5792, 5792, []byte{0x22, 0x11, 0x11}, 335472640, false},
		{"square 420 refused", 0xc2, 5793, 5793, []byte{0x22, 0x11, 0x11}, holdsMost, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := codexJPEGHeader(c.marker, c.w, c.h, byte(len(c.sampling)), c.sampling...)
			b64 := base64.StdEncoding.EncodeToString(raw)
			want := min(c.want+base64.StdEncoding.DecodedLen(len(b64)), holdsMost)
			got := fitHolds("data:image/jpeg;base64," + b64)
			if got != want || (got > codexImageMaxBytes) != c.refused {
				t.Fatalf("hold = %d, want %d; refused = %v, want %v", got, want, got > codexImageMaxBytes, c.refused)
			}
		})
	}
}

func TestCodexImageProgressiveRefusedBeforeDecode(t *testing.T) {
	for _, sampling := range [][]byte{{0x22, 0x11, 0x11}, {0x11, 0x11, 0x11}} {
		raw := codexJPEGHeader(0xc2, 8000, 10000, 3, sampling...)
		// A short Huffman table stops even an unguarded decoder before it
		// allocates pixels, so this regression stays small on the old code.
		raw = append(raw, codexJPEGSegment(0xc4, []byte{0})...)
		raw = append(raw, 0xff, 0xda, 0, 12)
		cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil || format != "jpeg" || cfg.Width != 8000 || cfg.Height != 10000 {
			t.Fatalf("header = %v, %q, %v", cfg, format, err)
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		if got := fitHolds("data:image/jpeg;base64," + b64); got <= codexImageMaxBytes {
			t.Errorf("progressive hold = %d, fits the decode budget", got)
		}
		before := refits.Load()
		e := codexImageRefit(b64, imageLimits{2048, 2500})
		if e.data != nil || e.mime != "" {
			t.Fatal("over-budget image did not go as it came")
		}
		if got := refits.Load() - before; got != 0 {
			t.Fatalf("reached the decoder %d times", got)
		}
	}
}

func TestFitHoldsProgressiveBehindAppSegments(t *testing.T) {
	raw := []byte{0xff, 0xd8}
	for _, marker := range []byte{0xe1, 0xe2, 0xe1} {
		raw = append(raw, codexJPEGSegment(marker, make([]byte, 60000))...)
	}
	header := codexJPEGHeader(0xc2, 6000, 4000, 3, 0x22, 0x11, 0x11)
	raw = append(raw, header[2:]...)
	b64 := base64.StdEncoding.EncodeToString(raw)
	want := base64.StdEncoding.DecodedLen(len(b64)) + 240000000
	if got := fitHolds("data:image/jpeg;base64," + b64); got != want {
		t.Fatalf("hold behind APP segments = %d, want %d", got, want)
	}
}

func TestJPEGProgressiveCoefficientSampling(t *testing.T) {
	for _, c := range []struct {
		name     string
		sampling []byte
		want     int64
	}{
		// At 20x17 the coefficients include the partially used edge MCUs.
		{"444", []byte{0x11, 0x11, 0x11}, 6912},
		{"420", []byte{0x22, 0x11, 0x11}, 6144},
		{"422", []byte{0x21, 0x11, 0x11}, 6144},
		{"440", []byte{0x12, 0x11, 0x11}, 6144},
		{"411", []byte{0x41, 0x11, 0x11}, 4608},
		{"410", []byte{0x42, 0x11, 0x11}, 5120},
		{"scaled 444", []byte{0x22, 0x22, 0x22}, 12288},
		{"grey", []byte{0x11}, 2304},
		{"grey nominal 44", []byte{0x44}, 2304},
		{"four components", []byte{0x22, 0x11, 0x11, 0x22}, 10240},
		{"zero horizontal", []byte{0x01, 0x11, 0x11}, 0},
		{"zero vertical", []byte{0x10, 0x11, 0x11}, 0},
		{"large horizontal", []byte{0x51, 0x11, 0x11}, 0},
		{"large vertical", []byte{0x15, 0x11, 0x11}, 0},
		{"unsupported horizontal", []byte{0x31}, 0},
		{"unsupported vertical", []byte{0x13}, 0},
		// Valid factors in unsupported layouts keep a bounded plain count.
		{"unequal chroma", []byte{0x22, 0x11, 0x21}, 7168},
		{"chroma larger than luma", []byte{0x11, 0x22, 0x22}, 9216},
		{"unsupported luma", []byte{0x44, 0x11, 0x11}, 4608},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := codexJPEGHeader(0xc2, 20, 17, byte(len(c.sampling)), c.sampling...)
			if got := jpegCoefficientHolds(bytes.NewReader(raw)); got != c.want {
				t.Fatalf("coefficient hold = %d, want %d", got, c.want)
			}
		})
	}
}

func TestFitHoldsProgressiveBehindExif(t *testing.T) {
	header := codexJPEGHeader(0xc2, 32, 24, 3)
	exif := make([]byte, 0xffff-2)
	copy(exif, "Exif\x00\x00")
	// A frame-looking sequence inside EXIF is not a frame marker.
	copy(exif[64:], codexJPEGHeader(0xc0, 1, 1, 3))
	raw := append([]byte{0xff, 0xd8}, codexJPEGSegment(0xe1, exif)...)
	raw = append(raw, header[2:]...)
	b64 := base64.StdEncoding.EncodeToString(raw)
	want := base64.StdEncoding.DecodedLen(len(b64)) + 16*32*24
	if got := fitHolds("data:image/jpeg;base64," + b64); got != want {
		t.Fatalf("hold behind EXIF = %d, want %d", got, want)
	}
}

func TestJPEGProgressiveCoefficientMarkers(t *testing.T) {
	header := codexJPEGHeader(0xc2, 32, 24, 3)
	frame := header[bytes.Index(header, []byte{0xff, 0xc2}):]
	for _, c := range []struct {
		name string
		raw  []byte
		want int64
	}{
		{"progressive", header, 4 * 3 * 32 * 24},
		{"capped", codexJPEGHeader(0xc2, 65535, 65535, 4), holdsMost},
		{"baseline first", append(codexJPEGHeader(0xc0, 32, 24, 3), frame...), 0},
		{"extended first", append(codexJPEGHeader(0xc1, 32, 24, 3), frame...), 0},
		{"lossless first", append(codexJPEGHeader(0xc3, 32, 24, 3), frame...), 0},
		{"scan first", append([]byte{0xff, 0xd8, 0xff, 0xda}, frame...), 0},
		{"end first", append([]byte{0xff, 0xd8, 0xff, 0xd9}, frame...), 0},
		{"fill", append([]byte{0xff, 0xd8, 0xff, 0xff}, frame...), 4 * 3 * 32 * 24},
		{"restart", append([]byte{0xff, 0xd8, 0xff, 0xd0}, frame...), 4 * 3 * 32 * 24},
		{"stray bytes", append([]byte{0xff, 0xd8, 1, 2, 3, 0xff, 0}, frame...), 4 * 3 * 32 * 24},
		{"bad length", append([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 1}, frame...), 0},
		{"short frame", []byte{0xff, 0xd8, 0xff, 0xc2, 0, 2}, 0},
		{"bad component length", []byte{0xff, 0xd8, 0xff, 0xc2, 0, 8, 8, 0, 24, 0, 32, 3}, 0},
		{"not JPEG", frame, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := jpegCoefficientHolds(bytes.NewReader(c.raw)); got != c.want {
				t.Fatalf("coefficient hold = %d, want %d", got, c.want)
			}
		})
	}
	for _, marker := range []byte{0xc4, 0xc8, 0xcc, 0xe1, 0xfe} {
		raw := append([]byte{0xff, 0xd8}, codexJPEGSegment(marker, frame)...)
		raw = append(raw, frame...)
		r := bytes.NewReader(append(raw, 1, 2, 3))
		if got := jpegCoefficientHolds(r); got != 4*3*32*24 || r.Len() != 3 {
			t.Errorf("marker %x: hold = %d, unread = %d", marker, got, r.Len())
		}
	}
	for end := 0; end < len(header); end++ {
		if got := jpegCoefficientHolds(bytes.NewReader(header[:end])); got != 0 {
			t.Fatalf("truncated at %d: hold = %d", end, got)
		}
	}
}

func TestCodexImageSmallProgressiveRefits(t *testing.T) {
	// Two grey blocks, with zero DC differences in the first scan and
	// zero AC coefficients in the second. Each Huffman code is one bit.
	raw := codexJPEGHeader(0xc2, 16, 8, 1)
	raw = append(raw, codexJPEGSegment(0xdb, append([]byte{0}, bytes.Repeat([]byte{1}, 64)...))...)
	for _, table := range []byte{0, 0x10} {
		payload := make([]byte, 18)
		payload[0], payload[1] = table, 1
		raw = append(raw, codexJPEGSegment(0xc4, payload)...)
	}
	for _, band := range [][2]byte{{0, 0}, {1, 63}} {
		raw = append(raw, codexJPEGSegment(0xda, []byte{1, 1, 0, band[0], band[1], 0})...)
		raw = append(raw, 0x3f)
	}
	raw = append(raw, 0xff, 0xd9)
	src, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil || src.Bounds() != image.Rect(0, 0, 16, 8) {
		t.Fatalf("progressive fixture failed to decode: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	if got, want := fitHolds("data:image/jpeg;base64,"+b64), base64.StdEncoding.DecodedLen(len(b64))+5*16*8; got != want {
		t.Errorf("small progressive hold = %d, want %d", got, want)
	}
	before := refits.Load()
	if e := codexImageRefit(b64, imageLimits{2048, 2500}); e.data != nil || refits.Load() != before {
		t.Fatal("an image that fits did not go as it came")
	}
	e := codexImageRefit(b64, imageLimits{8, 1})
	if e.mime != "image/jpeg" || e.data == nil || refits.Load() != before+1 {
		t.Fatal("small progressive image was not refitted as JPEG")
	}
	dst, err := jpeg.Decode(bytes.NewReader(e.data))
	if err != nil {
		t.Fatal(err)
	}
	if dst.Bounds() != image.Rect(0, 0, 8, 4) {
		t.Fatalf("refitted bounds = %v", dst.Bounds())
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 8; x++ {
			r, g, b, _ := dst.At(x, y).RGBA()
			if r != 0x8080 || g != r || b != r {
				t.Fatalf("pixel (%d, %d) = %d, %d, %d", x, y, r, g, b)
			}
		}
	}
}

// codexProgressiveColorJPEG makes a flat 512x512 image with an interleaved
// DC scan and one AC scan per component. Each zero difference or EOB is
// a one-bit Huffman code; the dimensions have no partial blocks or MCUs.
func codexProgressiveColorJPEG(sampling []byte) []byte {
	raw := codexJPEGHeader(0xc2, 512, 512, 3, sampling...)
	raw = append(raw, codexJPEGSegment(0xdb, append([]byte{0}, bytes.Repeat([]byte{1}, 64)...))...)
	for _, table := range []byte{0, 0x10} {
		payload := make([]byte, 18)
		payload[0], payload[1] = table, 1
		raw = append(raw, codexJPEGSegment(0xc4, payload)...)
	}
	mcus := (512 / (8 * int(sampling[0]>>4))) * (512 / (8 * int(sampling[0]&15)))
	var blocks [3]int
	for i, hv := range sampling {
		blocks[i] = mcus * int(hv>>4) * int(hv&15)
	}
	raw = append(raw, codexJPEGSegment(0xda, []byte{3, 1, 0, 2, 0, 3, 0, 0, 0, 0})...)
	raw = append(raw, make([]byte, (blocks[0]+blocks[1]+blocks[2])/8)...)
	for i, n := range blocks {
		raw = append(raw, codexJPEGSegment(0xda, []byte{1, byte(i + 1), 0, 1, 63, 0})...)
		raw = append(raw, make([]byte, n/8)...)
	}
	return append(raw, 0xff, 0xd9)
}

func TestJPEGProgressiveAllocation(t *testing.T) {
	if raceOn {
		t.Skip("allocation bound measures the decoder without race instrumentation")
	}
	for _, c := range []struct {
		name     string
		sampling []byte
	}{
		{"420", []byte{0x22, 0x11, 0x11}},
		{"444", []byte{0x11, 0x11, 0x11}},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := codexProgressiveColorJPEG(c.sampling)
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			pixels, ok := imageHolds(cfg, "jpeg")
			if !ok {
				t.Fatal("fixture has no pixel count")
			}
			hold := uint64(pixels) + uint64(jpegCoefficientHolds(bytes.NewReader(raw)))
			r := bytes.NewReader(raw)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			src, err := jpeg.Decode(r)
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("imageHolds + coefficients = %d bytes; jpeg.Decode TotalAlloc = %d bytes", hold, allocated)
			if err != nil {
				t.Fatal(err)
			}
			if hold < allocated {
				t.Fatalf("count %d is less than decoder allocations %d", hold, allocated)
			}
			if src.Bounds() != image.Rect(0, 0, 512, 512) {
				t.Fatalf("decoded bounds = %v", src.Bounds())
			}
			for _, p := range []image.Point{{0, 0}, {255, 255}, {511, 511}} {
				r, g, b, _ := src.At(p.X, p.Y).RGBA()
				if r != 0x8080 || g != r || b != r {
					t.Fatalf("pixel %v = %d, %d, %d", p, r, g, b)
				}
			}
		})
	}
}
