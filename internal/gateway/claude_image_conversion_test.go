package gateway

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

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
			if mt != "" || data != "" || !strings.Contains(note, "damaged") {
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
	claudeImageTestConfigs    atomic.Int32
)

func TestClaudeImageOtherRegisteredFormatIsNotDecoded(t *testing.T) {
	claudeImageTestFormatOnce.Do(func() {
		image.RegisterFormat("claudeimagetest", "CLIMGTEST", func(io.Reader) (image.Image, error) {
			claudeImageTestDecodes.Add(1)
			return image.NewRGBA(image.Rect(0, 0, 10, 10)), nil
		}, func(io.Reader) (image.Config, error) {
			claudeImageTestConfigs.Add(1)
			return image.Config{ColorModel: color.RGBAModel, Width: 10, Height: 10}, nil
		})
	})
	claudeImageTestDecodes.Store(0)
	claudeImageTestConfigs.Store(0)
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
	t.Logf("type %q, image %t, note %q; record %+v; decode calls %d, config calls %d; %s", mt, data != "", note, record, claudeImageTestDecodes.Load(), claudeImageTestConfigs.Load(), strings.TrimSpace(logs.String()))
	if mt != "" || data != "" || !strings.Contains(note, "format is not supported") {
		t.Error("registered format was not replaced by the note")
	}
	if !strings.Contains(logs.String(), "unsupported image type") {
		t.Error("missing unsupported image type reason")
	}
	if record.found != "" || record.width != 0 || record.height != 0 || record.action != "note" {
		t.Error("registered format record read a type or dimensions, or lost its action")
	}
	if calls := claudeImageTestDecodes.Load(); calls != 0 {
		t.Errorf("registered format decoded %d times, want 0", calls)
	}
	record = claudeImageReadRecord("image/png", encoded)
	if record.found != "" || record.width != 0 || record.height != 0 {
		t.Errorf("read record inspected unsupported header: %+v", record)
	}
	if calls := claudeImageTestConfigs.Load(); calls != 0 {
		t.Errorf("registered format config read %d times, want 0", calls)
	}
}

func TestClaudeImageDecodeBudget(t *testing.T) {
	var budget claudeImageByteBudget
	const capacity = codexImageMaxBytes
	if !budget.acquire(capacity/2, time.Second) || !budget.acquire(capacity/2, time.Second) {
		t.Fatal("two half-capacity decodes cannot run together")
	}
	done := make(chan bool, 1)
	go func() { done <- budget.acquire(1, time.Second) }()
	// Leave a wide margin before the acquire's one-second timeout.
	select {
	case ok := <-done:
		if ok {
			budget.release(1)
		}
		budget.release(capacity)
		t.Fatal("third decode did not wait for capacity")
	case <-time.After(50 * time.Millisecond):
	}
	budget.release(capacity / 2)
	if ok := <-done; !ok {
		budget.release(capacity / 2)
		t.Fatal("release did not wake the waiting decode")
	}
	budget.release(capacity/2 + 1)
	t.Log("two half-capacity acquires succeeded; the third waited and woke on release")

	// PNG, JPEG and BMP still decode even when the dimensions fit.
	oldWait, oldBusy := claudeImageConvertWait, claudeImageBusyWait
	claudeImageConvertWait, claudeImageBusyWait = 300*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { claudeImageConvertWait, claudeImageBusyWait = oldWait, oldBusy })
	for _, format := range []string{"bmp", "png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			if !claudeImageDecodeBudget.acquire(capacity, time.Second) {
				t.Fatal("decode budget was not empty")
			}
			held := true
			t.Cleanup(func() {
				if held {
					claudeImageDecodeBudget.release(capacity)
				}
			})
			raw := replayImage(t, format)
			declared := "image/" + format
			encoded := base64.StdEncoding.EncodeToString(raw)
			key := claudeImageClearFixture(t, declared, encoded)
			logs := keepLog(t)
			claudeImageDecodeBudget.mu.Lock()
			claudeImageDecodeBudget.gaveUp = time.Time{}
			claudeImageDecodeBudget.mu.Unlock()
			start := time.Now()
			mt, data, note := claudeImage(declared, encoded)
			elapsed := time.Since(start)
			t.Logf("occupied budget: %s, type %q, image %t, note %q; %s", elapsed, mt, data != "", note, strings.TrimSpace(logs.String()))
			if elapsed >= time.Second || mt != "" || data != "" || note != claudeImageOmittedBusy {
				t.Error("occupied budget did not return the busy note within a second")
			}
			if !strings.Contains(logs.String(), "conversion slot busy") {
				t.Error("missing conversion slot reason")
			}
			if _, cached := fitted.get(key); cached {
				t.Error("busy verdict was cached")
			}
			claudeImageDecodeBudget.mu.Lock()
			used := claudeImageDecodeBudget.used
			claudeImageDecodeBudget.mu.Unlock()
			if used != capacity {
				t.Errorf("give-up changed used to %d, want %d", used, capacity)
			}
			start = time.Now()
			record := claudeImageDetails(declared, encoded)
			if time.Since(start) >= claudeImageConvertWait {
				t.Error("details waited for the budget again")
			}
			sum := sha256.Sum256(raw)
			if record.action != "note" || record.found != declared || record.bytes != len(raw) || record.hash != fmt.Sprintf("%x", sum[:4]) {
				t.Errorf("busy record lost its action, type, bytes or hash: %+v", record)
			}
			if elapsed < claudeImageConvertWait {
				t.Error("first image did not wait the full interval")
			}
			start = time.Now()
			_, _, secondNote := claudeImage(declared, encoded)
			short := time.Since(start)
			t.Logf("second busy image waited %s", short)
			if secondNote != claudeImageOmittedBusy || short < claudeImageBusyWait || short >= claudeImageConvertWait {
				t.Error("give-up did not shorten the next wait")
			}
			claudeImageDecodeBudget.release(capacity)
			held = false
			mt, data, note = claudeImage(declared, encoded)
			want := declared
			if format == "bmp" {
				want = "image/png"
			}
			t.Logf("free budget: type %q, image %t, note %q", mt, data != "", note)
			if mt != want || data == "" || note != "" {
				t.Error("given-up verdict was kept instead of trying again")
			}
			if !claudeImageDecodeBudget.acquire(capacity, time.Second) {
				t.Fatal("decode left budget counted after returning")
			}
			claudeImageDecodeBudget.release(capacity)
		})
	}
	cfg := image.Config{Width: 8000, Height: 8000, ColorModel: color.RGBAModel}
	holds, _ := claudeImageHolds(cfg, "png", claudeImagePNGHeader(cfg.Width, cfg.Height, 8, 6, 0))
	weight := claudeImageDecodeWeight(cfg, holds, true)
	if weight <= capacity || budget.acquire(weight, time.Second) {
		t.Fatalf("over-capacity weight %d was admitted", weight)
	}
	t.Logf("over-capacity refit weight %d rejected; used %d", weight, budget.used)
	claudeImageDecodeBudget.mu.Lock()
	used := claudeImageDecodeBudget.used
	claudeImageDecodeBudget.mu.Unlock()
	if used != 0 {
		t.Errorf("shared budget left %d bytes used", used)
	}
}

// Measure each fixture cold, including when the test runs twice.
func claudeImageClearFixture(t *testing.T, declared, encoded string) [32]byte {
	t.Helper()
	key := fitKey(fmt.Sprintf("%d:%s%s", len(declared), declared, encoded), claudeImageLimits)
	clear := func() {
		fitted.Lock()
		defer fitted.Unlock()
		if el := fitted.m[key]; el != nil {
			fitted.drop(el)
		}
	}
	clear()
	t.Cleanup(clear)
	return key
}

func TestClaudeImageWebPFrameLargerThanCanvas(t *testing.T) {
	for _, side := range []uint32{4096, 1} {
		t.Run(fmt.Sprint(side), func(t *testing.T) {
			// VP8X gives a 1x1 canvas. Keep the tiny VP8L frame's entropy
			// coding, but replace its own width and height in the header.
			raw := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00"), make([]byte, 10)...)
			raw = append(raw, replayImage(t, "webp")[12:]...)
			binary.LittleEndian.PutUint32(raw[4:8], uint32(len(raw)-8))
			binary.LittleEndian.PutUint32(raw[39:43], side-1|(side-1)<<14)
			encoded := base64.StdEncoding.EncodeToString(raw)
			key := claudeImageClearFixture(t, "image/webp", encoded)
			logs := keepLog(t)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			mt, data, note := claudeImage("image/webp", encoded)
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			record := claudeImageDetails("image/webp", encoded)
			t.Logf("frame %dx%d, allocated %d bytes; type %q, image %t, note %q; record %+v; %s", side, side, allocated, mt, data != "", note, record, strings.TrimSpace(logs.String()))
			mismatch := strings.Contains(logs.String(), "WebP frame does not match its canvas")
			if side == 1 {
				if mismatch || mt != "image/webp" || data != encoded || note != "" {
					t.Error("matching frame did not go as it came")
				}
				return
			}
			if mt != "" || data != "" || !strings.Contains(note, "damaged") || !mismatch {
				t.Error("larger frame was not refused for its canvas mismatch")
			}
			if record.found != "image/webp" || record.width != 1 || record.height != 1 || record.action != "note" {
				t.Error("mismatch record lost its type, canvas or action")
			}
			if allocated >= 8<<20 {
				t.Errorf("mismatch allocated %d bytes, want less than 8 MiB", allocated)
			}
			if _, cached := fitted.get(key); !cached {
				t.Error("canvas mismatch verdict was not cached")
			}
		})
	}
}

func TestClaudeImageProgressiveJPEGCountsCoefficients(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker byte
		stray  bool
	}{{"SOF2", 0xc2, false}, {"SOF0", 0xc0, false}, {"SOF2 with stray bytes", 0xc2, true}} {
		t.Run(tc.name, func(t *testing.T) {
			marker := tc.marker
			// JFIF lets DecodeConfig finish at SOF without any scan data.
			raw := []byte{0xff, 0xd8}
			if tc.stray {
				// The JPEG decoder accepts stuffed, stray and fill bytes
				// before a marker; none may hide progressive coefficients.
				raw = append(raw, 0xff, 0, 0, 0xff)
			}
			raw = append(raw, 0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0)
			raw = append(raw, 0xff, marker, 0, 17, 8, 0x27, 0x10, 0x1f, 0x40, 3, 1, 0x11, 0, 2, 0x11, 0, 3, 0x11, 0, 0xff, 0xd9)
			cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
			if err != nil || format != "jpeg" || cfg.Width != 8000 || cfg.Height != 10000 {
				t.Fatalf("fixture header: %+v, %q, %v", cfg, format, err)
			}
			holds, ok := claudeImageHolds(cfg, format, raw)
			base, _ := imageHolds(cfg, format)
			want := 128<<10 + 3*8000*10000
			if marker == 0xc2 {
				want = min(128<<10+8000*10000*(3+12), holdsMost)
			}
			if !ok || holds != want {
				t.Errorf("holds %d, valid %t; want %d", holds, ok, want)
			}
			t.Logf("holds %d, baseline %d, capped at %d", holds, base, holdsMost)
			if marker == 0xc0 {
				return
			}
			// A smaller header also checks the component multiplier below the cap.
			small := cfg
			small.Width, small.Height = 800, 1000
			if n, _ := claudeImageHolds(small, format, raw); n != 128<<10+800*1000*(3+12) {
				t.Errorf("uncapped holds %d, want %d", n, 128<<10+800*1000*(3+12))
			}
			encoded := base64.StdEncoding.EncodeToString(raw)
			claudeImageClearFixture(t, "image/jpeg", encoded)
			logs := keepLog(t)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			mt, data, note := claudeImage("image/jpeg", encoded)
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("allocated %d bytes; type %q, image %t, note %q; %s", allocated, mt, data != "", note, strings.TrimSpace(logs.String()))
			problem, reason := "too large", "image too large to convert"
			if tc.stray {
				problem, reason = "damaged", "incomplete image container"
			}
			if mt != "" || data != "" || !strings.Contains(note, problem) || !strings.Contains(logs.String(), reason) {
				t.Error("progressive JPEG was not refused with the expected reason")
			}
			if allocated >= 8<<20 {
				t.Errorf("progressive refusal allocated %d bytes, want less than 8 MiB", allocated)
			}
		})
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
					if mt != "" || data != "" || !strings.Contains(note, "too large") {
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
			if mt != "" || data != "" || !strings.Contains(note, "too large") {
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

func claudeImageChunk(kind string, data []byte) []byte {
	b := append([]byte(kind), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(data)))
	b = append(b, data...)
	if len(data)%2 != 0 {
		b = append(b, 0)
	}
	return b
}

func claudeImageRIFF(chunks ...[]byte) []byte {
	b := []byte("WEBP")
	for _, chunk := range chunks {
		b = append(b, chunk...)
	}
	return claudeImageChunk("RIFF", b)
}

func TestClaudeImageWebPLeadingChunk(t *testing.T) {
	frame := append([]byte(nil), replayImage(t, "webp")[12:]...)
	binary.LittleEndian.PutUint32(frame[9:13], 4095|4095<<14)
	canvas := claudeImageChunk("VP8X", make([]byte, 10))
	for _, tc := range []struct {
		name, reason string
		raw          []byte
	}{
		{"leading VP8Z", "unsupported WebP layout", claudeImageRIFF(claudeImageChunk("VP8Z", nil), canvas, frame)},
		{"skipped odd JUNK", "WebP frame does not match its canvas", claudeImageRIFF(canvas, claudeImageChunk("JUNK", []byte{1}), frame)},
		{"missing frame", "WebP frame does not match its canvas", claudeImageRIFF(canvas, claudeImageChunk("ANMF", frame))},
		{"short frame", "WebP frame does not match its canvas", claudeImageRIFF(canvas, claudeImageChunk("VP8L", []byte{0x2f}))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(tc.raw)
			key := claudeImageClearFixture(t, "image/webp", encoded)
			logs := keepLog(t)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			mt, data, note := claudeImage("image/webp", encoded)
			runtime.ReadMemStats(&after)
			n := after.TotalAlloc - before.TotalAlloc
			t.Logf("allocated %d; type %q note %q; %s", n, mt, note, strings.TrimSpace(logs.String()))
			if n >= 8<<20 {
				t.Errorf("allocated %d, want <8 MiB", n)
			}
			if mt != "" || data != "" || !strings.Contains(note, "damaged") || !strings.Contains(logs.String(), tc.reason) {
				t.Error("unsafe layout did not receive its cached note")
			}
			if _, ok := fitted.get(key); !ok {
				t.Error("verdict not cached")
			}
		})
	}
}

type claudeImageBits struct {
	data []byte
	n    uint
}

func (b *claudeImageBits) put(v uint32, n uint) {
	for i := uint(0); i < n; i++ {
		if b.n%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[b.n/8] |= byte(v>>i&1) << (b.n % 8)
		b.n++
	}
}
func (b *claudeImageBits) single(v uint32) {
	b.put(1, 1)
	b.put(0, 1)
	b.put(1, 1)
	b.put(v, 8)
}

// A constant lossless image needs no pixel bits: each channel's tree has
// one symbol. The optional meta image selects group 16383, then EOF ends
// the main groups. Old decoders allocate about 42 MiB before that EOF.
func claudeImageLossless(w, h int, meta bool) []byte {
	b := claudeImageBits{}
	b.put(0x2f, 8)
	b.put(uint32(w-1), 14)
	b.put(uint32(h-1), 14)
	b.put(0, 4)
	b.put(0, 1) // transforms
	b.put(0, 1) // colour cache
	if meta {
		b.put(1, 1)
		b.put(0, 3)
		b.put(0, 1)
		for _, v := range []uint32{255, 63, 0, 255, 0} {
			b.single(v)
		}
	} else {
		b.put(0, 1)
		for _, v := range []uint32{0, 0, 0, 255, 0} {
			b.single(v)
		}
	}
	return b.data
}

func TestClaudeImageWebPNoRefitIsNotDecoded(t *testing.T) {
	raw := claudeImageRIFF(claudeImageChunk("VP8L", claudeImageLossless(1, 1, true)))
	encoded := base64.StdEncoding.EncodeToString(raw)
	claudeImageClearFixture(t, "image/webp", encoded)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	mt, data, note := claudeImage("image/webp", encoded)
	runtime.ReadMemStats(&after)
	n := after.TotalAlloc - before.TotalAlloc
	t.Logf("free budget: allocated %d, type %q, unchanged %t, note %q", n, mt, data == encoded, note)
	if mt != "image/webp" || data != encoded || note != "" || n >= 8<<20 {
		t.Error("complete WebP decoded or took the budget")
	}
	claudeImageClearFixture(t, "image/webp", encoded)
	// An occupied budget must not turn this complete small image into a busy note.
	if !claudeImageDecodeBudget.acquire(codexImageMaxBytes, time.Second) {
		t.Fatal("budget not empty")
	}
	defer claudeImageDecodeBudget.release(codexImageMaxBytes)
	mt, data, note = claudeImage("image/webp", encoded)
	if mt != "image/webp" || data != encoded || note != "" {
		t.Error("complete WebP took the occupied budget")
	}

}

func claudeImagePNGHeader(w, h int, depth, kind, interlace byte) []byte {
	b := make([]byte, 13)
	binary.BigEndian.PutUint32(b, uint32(w))
	binary.BigEndian.PutUint32(b[4:], uint32(h))
	b[8], b[9], b[12] = depth, kind, interlace
	return append([]byte("\x89PNG\r\n\x1a\n"), claudeImagePNGChunk("IHDR", b)...)
}
func claudeImagePNGChunk(kind string, b []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	out = append(out, kind...)
	out = append(out, b...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}
func claudeImagePNG(t *testing.T, w, h int, depth, kind, interlace byte, transparent bool) []byte {
	t.Helper()
	out := claudeImagePNGHeader(w, h, depth, kind, interlace)
	if transparent {
		out = append(out, claudeImagePNGChunk("tRNS", []byte{0, 0})...)
	}
	bpp := int(depth) / 8
	if kind == 6 {
		bpp *= 4
	}
	var data bytes.Buffer
	z := zlib.NewWriter(&data)
	passes := [][4]int{{0, 0, 1, 1}}
	if interlace != 0 {
		passes = [][4]int{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}}
	}
	for _, p := range passes {
		pw, ph := (w-p[0]+p[2]-1)/p[2], (h-p[1]+p[3]-1)/p[3]
		if pw <= 0 || ph <= 0 {
			continue
		}
		row := make([]byte, 1+pw*bpp)
		for range ph {
			if _, err := z.Write(row); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	out = append(out, claudeImagePNGChunk("IDAT", data.Bytes())...)
	return append(out, claudeImagePNGChunk("IEND", nil)...)
}

func TestClaudeImageOverCapacityIsNoted(t *testing.T) {
	raw := append(claudeImagePNGHeader(18000, 18000, 8, 0, 0), claudeImagePNGChunk("IEND", nil)...)
	encoded := base64.StdEncoding.EncodeToString(raw)
	key := claudeImageClearFixture(t, "image/png", encoded)
	logs := keepLog(t)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	mt, data, note := claudeImage("image/png", encoded)
	runtime.ReadMemStats(&after)
	n := after.TotalAlloc - before.TotalAlloc
	t.Logf("allocated %d; type %q, note %q; %s", n, mt, note, strings.TrimSpace(logs.String()))
	if mt != "" || data != "" || !strings.Contains(note, "too large") || !strings.Contains(logs.String(), "image too large to convert") || n >= 8<<20 {
		t.Error("over-capacity conversion was not refused before decode")
	}
	if _, ok := fitted.get(key); !ok {
		t.Error("refusal not cached")
	}
}

func TestClaudeImageHoldsHuffmanLayout(t *testing.T) {
	// The unexported x/image/vp8l hNode, hTree and hGroup layouts from
	// huffman.go and decode.go, including the compiled slice header.
	type node struct {
		symbol   uint32
		children int32
	}
	type tree struct {
		nodes []node
		lut   [128]uint32
	}
	type group [5]tree
	size := unsafe.Sizeof(group{})
	t.Logf("compiled hGroup = %d bytes; bound = %d", size, claudeImageHGroupBytes)
	if size > claudeImageHGroupBytes || unsafe.Sizeof(uintptr(0)) == 8 && size != claudeImageHGroupBytes {
		t.Fatal("Huffman group layout changed")
	}
	// Also measure the actual compiled decoder: the local layout above must
	// not silently stop matching x/image's unexported group type. This tiny
	// stream allocates 16384 groups and then fails before allocating pixels.
	raw := claudeImageRIFF(claudeImageChunk("VP8L", claudeImageLossless(1, 1, true)))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, err := image.Decode(bytes.NewReader(raw))
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	groups := 16384 * uint64(size)
	t.Logf("compiled decoder allocated %d; group storage %d", allocated, groups)
	if err == nil || allocated < groups || allocated > groups+128<<10 {
		t.Fatal("compiled decoder no longer matches the pinned group layout")
	}

}

func TestClaudeImageHoldsBound(t *testing.T) {
	fixtures := []struct {
		name string
		raw  []byte
	}{
		{"grey PNG", claudeImagePNG(t, 300, 200, 8, 0, 0, false)},
		{"grey PNG tRNS", claudeImagePNG(t, 300, 200, 8, 0, 0, true)},
		{"grey16 PNG tRNS", claudeImagePNG(t, 300, 200, 16, 0, 0, true)},
		{"interlaced RGBA PNG", claudeImagePNG(t, 300, 200, 8, 6, 1, false)},
		{"lossless WebP", claudeImageRIFF(claudeImageChunk("VP8L", claudeImageLossless(300, 200, false)))},
		{"BMP", claudeImageGray(t, "bmp", 300, 200)},
		{"GIF", replayImage(t, "gif")},
	}
	// Go's decoder regression corpus supplies colour spaces and progressive
	// scans that its JPEG encoder cannot produce. These are real small images.
	for _, name := range []string{"video-001.q50.420.jpeg", "video-001.q50.444.jpeg", "video-001.rgb.jpeg", "video-001.cmyk.jpeg", "video-001.progressive.jpeg"} {
		raw, err := os.ReadFile(filepath.Join(runtime.GOROOT(), "src", "image", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, struct {
			name string
			raw  []byte
		}{name, raw})
	}
	fixtures = append(fixtures, struct {
		name string
		raw  []byte
	}{"lossy WebP VP8L alpha", claudeImageAlphaWebP(t)})
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			cfg, format, err := image.DecodeConfig(bytes.NewReader(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			holds, ok := claudeImageHolds(cfg, format, tc.raw)
			if !ok {
				t.Fatal("no holds")
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			im, _, err := image.Decode(bytes.NewReader(tc.raw))
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatal(err)
			}
			n := after.TotalAlloc - before.TotalAlloc
			t.Logf("%dx%d %T: TotalAlloc %d <= holds %d", cfg.Width, cfg.Height, im, n, holds)
			if n > uint64(holds) {
				t.Errorf("allocation %d exceeds holds %d", n, holds)
			}
		})
	}
}

func claudeImageJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	// Spatial noise at quality 85 gives a phone-sized file, not just a header.
	v := uint32(1)
	for i := range im.Y {
		v ^= v << 13
		v ^= v >> 17
		v ^= v << 5
		im.Y[i] = 112 + byte(v&31)
	}
	for i := range im.Cb {
		im.Cb[i], im.Cr[i] = 128, 128
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, im, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestClaudeImageRefitWeightBound(t *testing.T) {
	for _, tc := range []struct {
		format string
		w, h   int
	}{{"png", 2001, 2001}, {"jpeg", 2600, 1800}, {"jpeg", 4032, 3024}, {"bmp", 1, 2073600}} {
		t.Run(fmt.Sprintf("%s/%dx%d", tc.format, tc.w, tc.h), func(t *testing.T) {
			var raw []byte
			switch tc.format {
			case "png":
				raw = claudeImagePNG(t, tc.w, tc.h, 8, 6, 0, false)
			case "jpeg":
				raw = claudeImageJPEG(t, tc.w, tc.h)
			case "bmp":
				raw = claudeImageGray(t, "bmp", tc.w, tc.h)
			}
			cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			holds, ok := claudeImageHolds(cfg, format, raw)
			if !ok {
				t.Fatal("no holds")
			}
			weight := claudeImageDecodeWeight(cfg, holds, true)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			src, _, err := image.Decode(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			e := claudeImageRefit(src, cfg, format)
			runtime.ReadMemStats(&after)
			n := after.TotalAlloc - before.TotalAlloc
			t.Logf("input %d, holds %d, weight %d, TotalAlloc %d; two fit = %t; result %s", len(raw), holds, weight, n, 2*weight <= codexImageMaxBytes, e.mime)
			if n > uint64(weight) || len(e.data) == 0 {
				t.Error("refit allocation exceeds weight or no output")
			}
			if tc.format != "bmp" && weight >= codexImageMaxBytes {
				t.Error("ordinary image exceeds capacity")
			}
			if tc.w == 4032 {
				encoded := base64.StdEncoding.EncodeToString(raw)
				claudeImageClearFixture(t, "image/jpeg", encoded)
				mt, data, note := claudeImage("image/jpeg", encoded)
				if mt != "image/jpeg" || data == encoded || data == "" || note != "" {
					t.Errorf("phone photo: type %q, unchanged %t, note %q", mt, data == encoded, note)
				}
			}
		})
	}
}

func TestClaudeImageEncodeBounded(t *testing.T) {
	im := image.NewRGBA(image.Rect(0, 0, 2000, 2000))
	v := uint32(1)
	for i := range im.Pix {
		v ^= v << 13
		v ^= v >> 17
		v ^= v << 5
		im.Pix[i] = byte(v)
	}
	out := claudeImageWriter{data: make([]byte, 0, claudeImageOutput)}
	err := png.Encode(&out, im)
	t.Logf("PNG stopped: %v; bytes %d capacity %d limit %d", err, len(out.data), cap(out.data), claudeImageOutput)
	if err == nil || cap(out.data) > claudeImageOutput || len(out.data) > claudeImageOutput {
		t.Fatal("PNG encode was not bounded")
	}
	e := claudeImageRefit(im, image.Config{Width: 2000, Height: 2000, ColorModel: color.RGBAModel}, "png")
	t.Logf("fallback type %s, output %d", e.mime, len(e.data))
	if e.data != nil && (e.mime != "image/jpeg" || base64.StdEncoding.EncodedLen(len(e.data)) > claudeImageBase64) {
		t.Fatal("fallback did not fit")
	}
}

func claudeImageAlphaWebP(t *testing.T) []byte {
	t.Helper()
	// x/image's blue-purple-pink.lossy.webp (150x100), with a constant
	// VP8L-coded alpha plane. The frame exercises the real lossy decoder.
	raw, err := base64.StdEncoding.DecodeString("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")
	if err != nil {
		t.Fatal(err)
	}
	canvas := []byte{16, 0, 0, 0, 149, 0, 0, 99, 0, 0}
	alpha := append([]byte{1}, claudeImageLossless(150, 100, false)[5:]...)
	return claudeImageRIFF(claudeImageChunk("VP8X", canvas), claudeImageChunk("ALPH", alpha), raw[12:])
}

func TestClaudeImageRefitWeightCapacity(t *testing.T) {
	for _, format := range []string{"jpeg", "png"} {
		var raw []byte
		if format == "jpeg" {
			raw = claudeImageJPEG(t, 8, 8)
		} else {
			raw = claudeImagePNGHeader(8, 8, 8, 6, 0)
		}
		weight := func(side int) int {
			cfg := image.Config{Width: side, Height: side, ColorModel: color.RGBAModel}
			holds, _ := claudeImageHolds(cfg, format, raw)
			return claudeImageDecodeWeight(cfg, holds, true)
		}
		lo, hi := 2000, 20000
		for lo+1 < hi {
			m := (lo + hi) / 2
			if weight(m) <= codexImageMaxBytes {
				lo = m
			} else {
				hi = m
			}
		}
		t.Logf("largest square %s (baseline 4:2:0 JPEG / 8-bit RGBA PNG): %dx%d weight %d; next %d", format, lo, lo, weight(lo), weight(hi))
		if weight(lo) > codexImageMaxBytes || weight(hi) <= codexImageMaxBytes {
			t.Fatal("capacity boundary")
		}
	}
	cfg := image.Config{Width: 1, Height: claudeImageConvertPixels, ColorModel: color.Palette{color.Black}}
	holds, _ := claudeImageHolds(cfg, "bmp", nil)
	weight := claudeImageDecodeWeight(cfg, holds, true)
	t.Logf("1x%d paletted BMP weight %d: over capacity, not decoded", cfg.Height, weight)
	if weight <= codexImageMaxBytes {
		t.Fatal("very tall BMP admitted")
	}
}

func TestClaudeImageAnimatedWebPFitsGoesAsItCame(t *testing.T) {
	raw := replayAnimation(t)
	for _, declared := range []string{"image/png", "image/webp"} {
		t.Run(declared, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(raw)
			claudeImageClearFixture(t, declared, encoded)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			mt, data, note := claudeImage(declared, encoded)
			runtime.ReadMemStats(&after)
			if mt != "image/webp" || data != encoded || note != "" {
				t.Fatalf("animation: type %q, unchanged %t, note %q", mt, data == encoded, note)
			}
			if n := after.TotalAlloc - before.TotalAlloc; n >= 1<<20 {
				t.Fatalf("animation allocated %d bytes", n)
			}
			claudeImageDecodeBudget.mu.Lock()
			used := claudeImageDecodeBudget.used
			claudeImageDecodeBudget.mu.Unlock()
			if used != 0 {
				t.Fatalf("budget used %d", used)
			}
			// An occupied budget makes even a temporary acquisition observable.
			if !claudeImageDecodeBudget.acquire(codexImageMaxBytes, time.Second) {
				t.Fatal("budget occupied")
			}
			defer claudeImageDecodeBudget.release(codexImageMaxBytes)
			claudeImageClearFixture(t, declared, encoded)
			if mt, data, note := claudeImage(declared, encoded); mt != "image/webp" || data != encoded || note != "" {
				t.Fatalf("animation took occupied budget: %q, %q", mt, note)
			}
		})
	}
	// The RIFF declares an odd chunk's data but omits its required padding.
	odd := claudeImageChunk("JUNK", []byte{1})
	malformed := append(bytes.Clone(raw), odd[:len(odd)-1]...)
	binary.LittleEndian.PutUint32(malformed[4:8], uint32(len(malformed)-8))
	mismatch := claudeImageRIFF(claudeImageChunk("VP8X", make([]byte, 10)), claudeImageChunk("VP8L", claudeImageLossless(4096, 4096, false)))
	trailing := append(bytes.Clone(raw), 0)
	binary.LittleEndian.PutUint32(trailing[4:8], uint32(len(trailing)-8))
	for _, bad := range [][]byte{malformed, mismatch, trailing} {
		encoded := base64.StdEncoding.EncodeToString(bad)
		claudeImageClearFixture(t, "image/webp", encoded)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		mt, data, note := claudeImage("image/webp", encoded)
		runtime.ReadMemStats(&after)
		if mt != "" || data != "" || !strings.Contains(note, "damaged") || after.TotalAlloc-before.TotalAlloc >= 8<<20 {
			t.Fatalf("malformed animation: type %q, note %q, allocated %d", mt, note, after.TotalAlloc-before.TotalAlloc)
		}
	}
}

func TestClaudeImageAnimatedWebPUnreadableHeaderIsNoted(t *testing.T) {
	raw := replayAnimation(t)
	// A 65536x32768 canvas fits WebP's area limit, but exceeds the
	// decoder's MaxInt32 limit. The animation chunks remain intact.
	copy(raw[24:27], []byte{0xff, 0xff, 0})
	copy(raw[27:30], []byte{0xff, 0x7f, 0})
	if _, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		t.Fatal("fixture must have an unreadable header")
	}
	if f, ok := claudeImageWebPFrame(raw); !ok || !f.noFrame || !claudeImageComplete("image/webp", raw) {
		t.Fatal("fixture must be a complete animation without a top-level frame")
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	claudeImageClearFixture(t, "image/webp", encoded)
	if !claudeImageDecodeBudget.acquire(codexImageMaxBytes, time.Second) {
		t.Fatal("budget occupied")
	}
	defer claudeImageDecodeBudget.release(codexImageMaxBytes)
	mt, data, note := claudeImage("image/webp", encoded)
	if mt != "" || data != "" || !strings.Contains(note, "damaged") {
		t.Fatalf("unreadable animation: type %q, note %q", mt, note)
	}
}

func TestClaudeImageHoldsRejectsWebPWithoutFrame(t *testing.T) {
	raw := replayAnimation(t)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := claudeImageWebPFrame(raw); !ok || !f.noFrame {
		t.Fatal("fixture must have no top-level frame")
	}
	if _, ok := claudeImageHolds(cfg, format, raw); ok {
		t.Fatal("animation without a frame counted as decodable")
	}
}

func TestClaudeImageRefitPNGWithoutIENDIsDamaged(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 2400, 1))); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()[:b.Len()-12]
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= claudeImageSide || claudeImageComplete("image/png", raw) {
		t.Fatalf("fixture must need refitting and lack IEND: %v", err)
	}
	holds, ok := claudeImageHolds(cfg, format, raw)
	if !ok || claudeImageDecodeWeight(cfg, holds, true) > codexImageMaxBytes || holds > claudeImageDecodeLimit-len(raw) {
		t.Fatal("fixture must reach image.Decode")
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err == nil {
		t.Fatal("fixture must fail image.Decode")
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	claudeImageClearFixture(t, "image/png", encoded)
	mt, data, note := claudeImage("image/png", encoded)
	if mt != "" || data != "" || !strings.Contains(note, "2400x1 PNG") || !strings.Contains(note, "damaged") || strings.Contains(note, "too large") {
		t.Fatalf("incomplete refit PNG: type %q, note %q", mt, note)
	}
}

func TestClaudeImageUnconvertibleRefitGoesAsItCame(t *testing.T) {
	raw := claudeImageRIFF(claudeImageChunk("VP8L", claudeImageLossless(2400, 1600, false)))
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	holds, ok := claudeImageHolds(cfg, format, raw)
	if !ok || claudeImageDecodeWeight(cfg, holds, true) <= codexImageMaxBytes {
		t.Fatal("fixture must exceed conversion capacity")
	}
	// Check the generated bit stream, outside the measured gateway call.
	if im, _, err := image.Decode(bytes.NewReader(raw)); err != nil || im.Bounds().Dx() != 2400 || im.Bounds().Dy() != 1600 {
		t.Fatalf("lossless fixture: %v", err)
	}
	animation := replayAnimation(t)
	// Canvas 2400x1600, still no top-level frame.
	animation[24], animation[25] = 0x5f, 9
	animation[27], animation[28] = 0x3f, 6
	for _, good := range [][]byte{raw, animation} {
		encoded := base64.StdEncoding.EncodeToString(good)
		claudeImageClearFixture(t, "image/png", encoded)
		logs := keepLog(t)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		mt, data, note := claudeImage("image/png", encoded)
		runtime.ReadMemStats(&after)
		if mt != "image/webp" || data != encoded || note != "" || after.TotalAlloc-before.TotalAlloc >= 8<<20 {
			t.Errorf("unconvertible image: type %q, unchanged %t, note %q, allocated %d", mt, data == encoded, note, after.TotalAlloc-before.TotalAlloc)
		}
		if !strings.Contains(logs.String(), "image needs a refit the gateway cannot do; sent as it came") {
			t.Error("missing pass-through reason")
		}
		if !claudeImageDecodeBudget.acquire(codexImageMaxBytes, time.Second) {
			t.Fatal("budget occupied")
		}
		claudeImageClearFixture(t, "image/png", encoded)
		mt, data, note = claudeImage("image/png", encoded)
		claudeImageDecodeBudget.release(codexImageMaxBytes)
		if mt != "image/webp" || data != encoded || note != "" {
			t.Errorf("unconvertible image took budget: %q, %q", mt, note)
		}
	}
	bmp := claudeImageGray(t, "bmp", 1, 1)
	binary.LittleEndian.PutUint32(bmp[18:], 4000)
	binary.LittleEndian.PutUint32(bmp[22:], 4000)
	for _, bad := range [][]byte{
		claudeImageRIFF(claudeImageChunk("VP8L", claudeImageLossless(8001, 1600, false))),
		claudeImageRIFF(raw[12:], claudeImageChunk("JUNK", make([]byte, claudeImageOutput))),
		bmp,
	} {
		encoded := base64.StdEncoding.EncodeToString(bad)
		claudeImageClearFixture(t, "image/png", encoded)
		mt, data, note := claudeImage("image/png", encoded)
		if mt != "" || data != "" || !strings.Contains(note, "too large") {
			t.Fatalf("over-limit image: %q, %q", mt, note)
		}
	}
}

func TestClaudeImageRefitPNGOverDecodeLimitIsNoted(t *testing.T) {
	raw := append(claudeImagePNGHeader(8, 8, 8, 6, 0), claudeImagePNGChunk("tEXt", make([]byte, claudeImageOutput))...)
	raw = append(raw, claudeImagePNGChunk("IEND", nil)...)
	encoded := base64.StdEncoding.EncodeToString(raw)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	holds, ok := claudeImageHolds(cfg, format, raw)
	if !ok || claudeImageDecodeWeight(cfg, holds, true) > codexImageMaxBytes || len(encoded) <= claudeImageBase64 {
		t.Fatal("fixture must reach raw-plus-decode limit")
	}
	// Lower only the raw-plus-decode guard; keep the shared budget and
	// conversion weight unchanged so a small fixture reaches this branch.
	saved := claudeImageDecodeLimit
	claudeImageDecodeLimit = holds + len(raw) - 1
	t.Cleanup(func() { claudeImageDecodeLimit = saved })
	claudeImageClearFixture(t, "image/png", encoded)
	logs := keepLog(t)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	mt, data, note := claudeImage("image/png", encoded)
	runtime.ReadMemStats(&after)
	if mt != "" || data != "" || !strings.Contains(note, "too large") || !strings.Contains(logs.String(), "image exceeds the decode limit") {
		t.Fatalf("over-limit PNG: type %q, note %q; %s", mt, note, logs.String())
	}
	if n := after.TotalAlloc - before.TotalAlloc; n >= 32<<20 {
		t.Fatalf("PNG allocated %d bytes", n)
	}
}

func TestClaudeImageNotesSayWhy(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0,
		0xff, 0xc2, 0, 17, 8, 0x27, 0x10, 0x1f, 0x40, 3, 1, 0x11, 0, 2, 0x11, 0, 3, 0x11, 0, 0xff, 0xd9}
	for _, tc := range []struct{ name, encoded, want string }{
		{"too large", base64.StdEncoding.EncodeToString(jpeg), "[An image was left out: this 8000x10000 JPEG (1 KB) is too large for the gateway to convert; send it again at most 2000 px a side]"},
		{"damaged", base64.StdEncoding.EncodeToString(claudeImagePNGHeader(300, 200, 8, 6, 0)), "[An image was left out: this 300x200 PNG (1 KB) is damaged or incomplete and could not be read; send it again]"},
		{"invalid base64", "%%%", "[An image was left out: this image is damaged or incomplete and could not be read; send it again]"},
		{"unsupported", base64.StdEncoding.EncodeToString([]byte("not an image")), "[An image was left out: its format is not supported here; send it as PNG or JPEG]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := claudeImageClearFixture(t, "image/png", tc.encoded)
			for range 2 {
				mt, data, note := claudeImage("image/png", tc.encoded)
				if mt != "" || data != "" || note != tc.want {
					t.Errorf("type %q, note %q, want %q", mt, note, tc.want)
				}
				if len(note) >= 200 {
					t.Errorf("note is %d bytes", len(note))
				}
				for _, c := range []byte(note) {
					if c > 127 {
						t.Fatal("non-ASCII note")
					}
				}
				if e, ok := fitted.get(key); !ok || string(e.data) != note {
					t.Error("note not stored with cached verdict")
				}
			}
		})
	}
}
