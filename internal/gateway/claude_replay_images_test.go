package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

func replayImage(t *testing.T, format string) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			m.Set(x, y, color.RGBA{uint8(x * 31), uint8(y * 31), 127, 255})
		}
	}
	var b bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, m)
	case "jpeg":
		err = jpeg.Encode(&b, m, nil)
	case "gif":
		err = gif.Encode(&b, m, nil)
	case "bmp":
		err = bmp.Encode(&b, m)
	case "tiff":
		err = tiff.Encode(&b, m, nil)
	case "webp":
		// A lossless 1x1 WebP, also used as the frame of the animation below.
		return []byte{'R', 'I', 'F', 'F', 28, 0, 0, 0, 'W', 'E', 'B', 'P', 'V', 'P', '8', 'L', 15, 0, 0, 0, 0x2f, 0, 0, 0, 0, 7, 0x10, 0xfd, 0x8f, 0xfe, 7, 0x22, 0xa2, 0xff, 1, 0}
	default:
		t.Fatalf("unknown format %s", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func replayAnimation(t *testing.T) []byte {
	t.Helper()
	chunk := func(name string, b []byte) []byte {
		out := append([]byte(name), make([]byte, 4)...)
		binary.LittleEndian.PutUint32(out[4:], uint32(len(b)))
		out = append(out, b...)
		if len(b)%2 != 0 {
			out = append(out, 0)
		}
		return out
	}
	// VP8X says animation, 1x1 canvas. ANMF holds a complete VP8L frame.
	body := append([]byte("WEBP"), chunk("VP8X", []byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0})...)
	body = append(body, chunk("ANIM", make([]byte, 6))...)
	frame := make([]byte, 16)
	frame[12] = 100
	frame = append(frame, replayImage(t, "webp")[12:]...)
	body = append(body, chunk("ANMF", frame)...)
	return chunk("RIFF", body)
}

func TestClaudeImageRealBytes(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif", "webp"} {
		raw := replayImage(t, format)
		if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
			t.Fatalf("%s fixture: %v", format, err)
		}
		data := base64.StdEncoding.EncodeToString(raw)
		for _, declared := range []string{"image/png", "image/jpeg", "image/gif", "image/webp", "", "image/jpg", "IMAGE/PNG", "image/png; charset=binary"} {
			t.Run(format+"/"+declared, func(t *testing.T) {
				for range 2 { // the cached verdict is the same, for each declared type
					mt, d, note := claudeImage(declared, data)
					if mt != "image/"+format || d != data || note != "" {
						t.Fatalf("got type %q, unchanged %v, note %q", mt, d == data, note)
					}
				}
				if got := imageMediaType(Part{MediaType: declared, Data: data}); got != "image/"+format {
					t.Fatalf("MCP sniff: %q", got)
				}
			})
		}
	}
}

func TestClaudeImageTrailingBytesGoAsTheyCame(t *testing.T) {
	for _, tc := range []struct {
		format, declared string
		padding          byte
	}{
		{"png", "image/png", 0xa5},
		{"jpeg", "image/jpeg", 0xa5},
		{"gif", "image/gif", 0},
		{"png", "image/jpeg", 0xa5},
	} {
		t.Run(tc.format+"/"+tc.declared, func(t *testing.T) {
			raw := append(replayImage(t, tc.format), bytes.Repeat([]byte{tc.padding}, 16)...)
			if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
				t.Fatalf("fixture must decode: %v", err)
			}
			data := base64.StdEncoding.EncodeToString(raw)
			for range 2 {
				mt, d, note := claudeImage(tc.declared, data)
				if mt != "image/"+tc.format || d != data || note != "" {
					t.Fatalf("got type %q, unchanged %v, note %q", mt, d == data, note)
				}
			}
		})
	}
}

func TestClaudeImageGIFTruncatedSecondFrameIsOmitted(t *testing.T) {
	frame := image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.Black, color.White})
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{0, 0}}); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()
	if decoded, err := gif.DecodeAll(bytes.NewReader(raw)); err != nil || len(decoded.Image) != 2 {
		t.Fatalf("fixture must have two frames: %v", err)
	}
	// Remove the last compressed byte, sub-block terminator and trailer.
	raw = raw[:len(raw)-3]
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("first frame must still decode: %v", err)
	}
	if _, err := gif.DecodeAll(bytes.NewReader(raw)); err == nil {
		t.Fatal("second frame must be incomplete")
	}
	mt, data, note := claudeImage("image/gif", base64.StdEncoding.EncodeToString(raw))
	if mt != "" || data != "" || note != claudeImageOmitted {
		t.Fatalf("truncated GIF passed: %q, note %q", mt, note)
	}
}

func TestClaudeImagePNGWithoutIENDIsOmitted(t *testing.T) {
	raw := replayImage(t, "png")
	if !bytes.Equal(raw[len(raw)-12:], []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82}) {
		t.Fatal("fixture must end with an empty IEND chunk")
	}
	raw = raw[:len(raw)-12]
	mt, data, note := claudeImage("image/png", base64.StdEncoding.EncodeToString(raw))
	if mt != "" || data != "" || note != claudeImageOmitted {
		t.Fatalf("PNG without IEND passed: %q, note %q", mt, note)
	}
}

func TestClaudeImageBrokenOrUnreadable(t *testing.T) {
	raw := replayImage(t, "png")
	truncated := raw[:len(raw)*55/100]
	if _, err := png.DecodeConfig(bytes.NewReader(truncated)); err != nil {
		t.Fatalf("fixture must have a readable header: %v", err)
	}
	cases := map[string]string{"truncated PNG": base64.StdEncoding.EncodeToString(truncated), "random": base64.StdEncoding.EncodeToString([]byte("not an image")), "SVG": base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)), "invalid base64": "%%%", "empty": "", "decoded empty": "\r\n"}
	for _, format := range []string{"jpeg", "gif", "webp"} {
		b := replayImage(t, format)
		cases["truncated "+format] = base64.StdEncoding.EncodeToString(b[:len(b)-3])
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			for range 2 {
				mt, d, note := claudeImage("image/png", data)
				if mt != "" || d != "" || note != claudeImageOmitted {
					t.Fatalf("got %q, %q, %q", mt, d, note)
				}
			}
		})
	}
}

func TestClaudeImageOtherFormatsAsPNG(t *testing.T) {
	for _, format := range []string{"bmp", "tiff"} {
		t.Run(format, func(t *testing.T) {
			raw := replayImage(t, format)
			original, _, err := image.Decode(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			mt, data, note := claudeImage("image/"+format, base64.StdEncoding.EncodeToString(raw))
			if mt != "image/png" || note != "" {
				t.Fatalf("type %q note %q", mt, note)
			}
			got, err := png.Decode(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
			if err != nil {
				t.Fatal(err)
			}
			if got.Bounds() != original.Bounds() {
				t.Fatal("bounds changed")
			}
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					if color.RGBAModel.Convert(got.At(x, y)) != color.RGBAModel.Convert(original.At(x, y)) {
						t.Fatalf("pixel %d,%d changed", x, y)
					}
				}
			}
		})
	}
}

func TestClaudeImageCompleteAnimationGoesAsItCame(t *testing.T) {
	raw := replayAnimation(t)
	if _, _, err := image.Decode(bytes.NewReader(raw)); err == nil {
		t.Fatal("fixture must exercise a format Go cannot decode")
	}
	data := base64.StdEncoding.EncodeToString(raw)
	mt, d, note := claudeImage("image/png", data)
	if mt != "image/webp" || d != data || note != "" {
		t.Fatalf("got %q, unchanged %v, note %q", mt, d == data, note)
	}
}

func TestClaudeReplayImagesInTranscript(t *testing.T) {
	for _, tc := range []struct {
		name, format, declared string
		tool, broken, only     bool
	}{
		{"tool JPEG", "jpeg", "image/png", true, false, false},
		{"tool WebP", "webp", "image/png", true, false, false},
		{"user JPEG", "jpeg", "image/png", false, false, false},
		{"broken tool PNG", "png", "image/png", true, true, false},
		{"broken image alone", "png", "image/png", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tuiScriptHarness(t, `[ -f "$file" ] && cp "$file" "$file.seen"`)
			raw := replayImage(t, tc.format)
			if tc.broken {
				raw = raw[:len(raw)*55/100]
			}
			im := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": tc.declared, "data": base64.StdEncoding.EncodeToString(raw)}}
			user := func(content any) map[string]any { return map[string]any{"role": "user", "content": content} }
			assistant := func(content any) map[string]any { return map[string]any{"role": "assistant", "content": content} }
			var messages []map[string]any
			if tc.tool {
				messages = []map[string]any{user("look"), assistant([]any{map[string]any{"type": "tool_use", "id": "c1", "name": "read", "input": map[string]any{}}}), user([]any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": []any{map[string]any{"type": "text", "text": "tool's own text"}, im}}})}
			} else {
				content := []any{im}
				if !tc.only {
					content = append([]any{map[string]any{"type": "text", "text": "look"}}, content...)
				}
				messages = []map[string]any{user(content)}
			}
			messages = append(messages, assistant("a picture"), user("and its title?"))
			body, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "max_tokens": 100, "tools": []any{map[string]any{"name": "read", "input_schema": map[string]any{"type": "object"}}}, "messages": messages})
			var u Usage
			if code, msg := h.s.serveClaudeSubscription(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body)), provider.Anthropic, h.p, "claude-sonnet-5", body, &u); code != 200 {
				t.Fatalf("%d %s", code, msg)
			}
			files, _ := filepath.Glob(filepath.Join(h.config, "projects", "*", "*.jsonl.seen"))
			if len(files) != 1 {
				t.Fatalf("%d transcripts", len(files))
			}
			b, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var row struct {
				Message struct {
					Content []map[string]any `json:"content"`
				} `json:"message"`
			}
			if err = json.Unmarshal(bytes.Split(b, []byte("\n"))[0], &row); err != nil {
				t.Fatal(err)
			}
			images, notes := 0, 0
			for _, block := range row.Message.Content {
				if block["type"] == "image" {
					images++
					src := block["source"].(map[string]any)
					if src["media_type"] != "image/"+tc.format || src["data"] != base64.StdEncoding.EncodeToString(raw) {
						t.Fatalf("wrong image: %v", src)
					}
				}
				if block["type"] == "text" && block["text"] == claudeImageOmitted {
					notes++
				}
			}
			if tc.broken {
				if images != 0 || notes != 1 {
					t.Fatalf("images %d notes %d", images, notes)
				}
			} else if images != 1 || notes != 0 {
				t.Fatalf("images %d notes %d", images, notes)
			}
			if tc.tool && !bytes.Contains(b, []byte("tool's own text")) {
				t.Fatal("tool text lost")
			}
			if len(row.Message.Content) == 0 {
				t.Fatal("empty user message")
			}
		})
	}
}

func TestClaudeImageMCPResult(t *testing.T) {
	for _, broken := range []bool{false, true} {
		raw := replayImage(t, "jpeg")
		if broken {
			raw = raw[:len(raw)/2]
		}
		data := base64.StdEncoding.EncodeToString(raw)
		result := mcpResult(Part{Text: "tool text", Images: []Part{{MediaType: "image/png", Data: data}}})
		if len(result.Content) != 2 || result.Content[0]["text"] != "tool text" {
			t.Fatalf("content %v", result.Content)
		}
		got := result.Content[1]
		if broken {
			if got["type"] != "text" || got["text"] != claudeImageOmitted {
				t.Fatalf("broken image: %v", got)
			}
		} else if got["type"] != "image" || got["mimeType"] != "image/jpeg" || got["data"] != data {
			t.Fatalf("mislabelled image: %v", got)
		}
	}
}

func TestClaudeImageThumbnailDoesNotEndATruncatedJPEG(t *testing.T) {
	raw := replayImage(t, "jpeg")
	// An APP1 segment containing a thumbnail's EOI, before the main image.
	raw = append(append(append([]byte{}, raw[:2]...), 0xff, 0xe1, 0, 6, 0xff, 0xd9, 0, 0), raw[2:len(raw)-16]...)
	if _, err := jpeg.DecodeConfig(bytes.NewReader(raw)); err != nil {
		t.Fatalf("fixture header: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(raw)); err == nil {
		t.Fatal("fixture must be incomplete")
	}
	mt, data, note := claudeImage("image/jpeg", base64.StdEncoding.EncodeToString(raw))
	if mt != "" || data != "" || note != claudeImageOmitted {
		t.Fatalf("truncated JPEG passed: %q, note %q", mt, note)
	}
}

func TestClaudeImageCacheSeparatesTheDeclaredType(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(replayImage(t, "png"))
	// These pairs collided when the key joined them with a semicolon.
	mt, d, note := claudeImage("image/png;charset=binary", data)
	if mt != "image/png" || d != data || note != "" {
		t.Fatalf("valid image: %q, %q", mt, note)
	}
	mt, d, note = claudeImage("image/png", "charset=binary;"+data)
	if mt != "" || d != "" || note != claudeImageOmitted {
		t.Fatal("invalid base64 borrowed the valid image's verdict")
	}
}

func TestClaudeImageDiagnosticRecord(t *testing.T) {
	for _, format := range []string{"jpeg", "png", "bmp"} {
		raw := replayImage(t, format)
		action := "as-is"
		if format == "png" {
			raw = raw[:len(raw)*55/100]
			action = "note"
		}
		if format == "bmp" {
			action = "re-encoded"
		}
		declared := "image/png"
		data := base64.StdEncoding.EncodeToString(raw)
		claudeImage(declared, data)
		record := claudeImageDetails(declared, data)
		sum := sha256.Sum256(raw)
		found := "image/" + format
		if record.declared != declared || record.found != found || record.bytes != len(raw) || record.width != 8 || record.height != 8 || record.hash != fmt.Sprintf("%x", sum[:4]) || record.action != action {
			t.Fatalf("%s record: %+v", format, record)
		}
		// A cached verdict must still supply a record after record eviction.
		key := fitKey(fmt.Sprint(len(declared))+":"+declared+data, claudeImageLimits)
		claudeImageRecords.Lock()
		delete(claudeImageRecords.m, key)
		claudeImageRecords.Unlock()
		if again := claudeImageDetails(declared, data); again != record {
			t.Fatalf("record after eviction: %+v, want %+v", again, record)
		}
	}
}

func TestMediaRemovedLogsReplayImages(t *testing.T) {
	logs := keepLog(t)
	h := tuiScriptHarness(t, mediaRemovedScript(mediaRemovedNotice))
	jpeg := replayImage(t, "jpeg")
	png := replayImage(t, "png")
	png = png[:len(png)*55/100]
	images := []string{base64.StdEncoding.EncodeToString(jpeg), base64.StdEncoding.EncodeToString(png)}
	body := fmt.Sprintf(`{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"private conversation sentinel"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]},{"role":"assistant","content":"a picture"},{"role":"user","content":"describe it"}]}`, images[0], images[1])
	var u Usage
	if code, msg := h.s.serveClaudeSubscription(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, h.p, "claude-sonnet-5", []byte(body), &u); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	h.s.subscription.abortAll()
	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "images handed over") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want one image inventory, got %v", lines)
	}
	for i, raw := range [][]byte{jpeg, png} {
		found, action := "image/jpeg", "as-is"
		if i == 1 {
			found, action = "image/png", "note"
		}
		hash := sha256.Sum256(raw)
		want := fmt.Sprintf(`[%d] declared "image/png" found %q %d bytes 8x8 sha256 %x %s`, i+1, found, len(raw), hash[:4], action)
		if !strings.Contains(lines[0], want) {
			t.Fatalf("missing %s in %s", want, lines[0])
		}
		if strings.Contains(logs.String(), images[i][:32]) {
			t.Fatal("image bytes leaked to the log")
		}
	}
	if !strings.Contains(lines[0], "2 images handed over") || strings.Contains(logs.String(), "private conversation sentinel") {
		t.Fatalf("unsafe or wrong inventory: %s", logs.String())
	}
	t.Log(lines[0])
}

func TestClaudeImageRecordsStayBounded(t *testing.T) {
	logs := keepLog(t)
	run := &subscriptionRun{}
	data := base64.StdEncoding.EncodeToString(replayImage(t, "jpeg"))
	for i := range 70 {
		run.noteImages([]Part{{Kind: Image, MediaType: fmt.Sprintf("image/%d", i), Data: data}})
	}
	run.mu.Lock()
	n, dropped := len(run.images), run.imagesDropped
	first, last := run.images[0].declared, run.images[n-1].declared
	run.mu.Unlock()
	if n != 64 || dropped != 6 || first != "image/6" || last != "image/69" {
		t.Fatalf("records %d dropped %d first %q last %q", n, dropped, first, last)
	}
	run.logImages()
	if !strings.Contains(logs.String(), "70 images handed over (6 not listed)") {
		t.Fatal(logs.String())
	}
}

func TestClaudeImageDeclaredTypeIsBoundedInLogs(t *testing.T) {
	logs := keepLog(t)
	declared := strings.Repeat("x", 64) + "hidden-label-tail\n"
	for _, format := range []string{"png", "bmp"} {
		raw := replayImage(t, format)
		if format == "png" {
			raw = raw[:len(raw)*55/100]
		}
		data := base64.StdEncoding.EncodeToString(raw)
		claudeImage(declared, data)
		record := claudeImageDetails(declared, data)
		if len(record.declared) != 64 {
			t.Fatalf("declared type: %q", record.declared)
		}
		sum := sha256.Sum256(raw)
		if !strings.Contains(logs.String(), fmt.Sprintf("8x8 sha256 %x", sum[:4])) {
			t.Fatalf("missing dimensions/hash: %s", logs.String())
		}
	}
	if strings.Contains(logs.String(), "hidden-label-tail") {
		t.Fatal("unbounded declared type in log")
	}
}

func TestMediaRemovedLogsToolResultImages(t *testing.T) {
	logs := keepLog(t)
	waiter := make(chan mcpToolResult, 1)
	run := &subscriptionRun{bridge: newSubscriptionBridge(), pending: map[string]chan mcpToolResult{"c1": waiter}}
	raw := replayImage(t, "jpeg")
	data := base64.StdEncoding.EncodeToString(raw)
	segment, err := run.continueWith([]Part{{Kind: ToolResult, CallID: "c1", Images: []Part{{MediaType: "image/png", Data: data}}}}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	result := <-waiter
	if result.Content[1]["mimeType"] != "image/jpeg" {
		t.Fatalf("tool image: %v", result.Content)
	}
	run.readOutput(strings.NewReader(mediaRemovedNotice + "\n" + mediaRemovedResult + "\n"))
	for range segment {
	}
	sum := sha256.Sum256(raw)
	want := fmt.Sprintf(`1 images handed over: [1] declared "image/png" found "image/jpeg" %d bytes 8x8 sha256 %x as-is`, len(raw), sum[:4])
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("missing tool image: %s", logs.String())
	}
}
