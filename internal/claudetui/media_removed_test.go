package claudetui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func mediaRemovedRow(heals bool, stop, details string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "isApiErrorMessage": true, "apiError": "media_removed",
		"apiErrorParams": map[string]any{"media": "image", "media_reason": "unprocessable"},
		"error":          "invalid_request", "errorDetails": details, "healsDistinctCarrier": heals,
		"message": map[string]any{"model": "<synthetic>", "role": "assistant", "stop_reason": stop,
			"content": []any{map[string]any{"type": "text", "text": "API Error: an image in the conversation could not be processed and was removed. Double press esc to edit."}}},
	})
	return b
}

func TestMediaRemovedHealingKeepsTheStream(t *testing.T) {
	for _, heals := range []bool{false, true} {
		for _, started := range []bool{false, true} {
			t.Run(fmt.Sprintf("heals=%v/stream-first=%v", heals, started), func(t *testing.T) {
				l := &lines{}
				tp, write := testTap(t, l)
				tr := newTranslator(l)
				tr.tap = tp
				stop := ""
				if !heals {
					stop = "tool_use"
				}
				reply := tapReply(1, "m1")
				write(`{"tap":"on"}`)
				if started {
					write(`{"s":1,"open":true}`, reply[0], reply[1], reply[2])
					tp.read()
				}
				before := len(l.out)
				tr.row(mediaRemovedRow(heals, stop, `{"error":{"message":"Could not process image"}}`))
				if tr.failed || l.overed != 0 || len(l.out) != before+1 {
					t.Fatalf("healing ended or settled the turn: %+v, over %d", l.out, l.overed)
				}
				notice := l.out[before]
				if notice["type"] != "media_removed" || notice["result"] != "Could not process image" {
					t.Fatalf("notice %v", notice)
				}
				if started {
					write(reply[3:]...)
				} else {
					write(`{"s":1,"open":true}`)
					write(reply...)
				}
				tr.row(whole("m1"))
				tr.row([]byte(`{"type":"system","subtype":"turn_duration"}`))
				got := []string{}
				for _, k := range kinds(l) {
					if k != "media_removed" {
						got = append(got, k)
					}
				}
				if !slices.Equal(got, streamedTurn) || l.overed != 1 {
					t.Fatalf("reply %v, over %d", got, l.overed)
				}
				for _, line := range l.out {
					if line["is_error"] == true {
						t.Fatalf("failed: %v", line)
					}
				}
			})
		}
	}
}

func TestMediaRemovedTerminalError(t *testing.T) {
	for _, tc := range []struct {
		name, details, want string
		legacy              bool
	}{
		{"JSON", `{"error":{"message":"Could not process image"}}`, "Could not process image", false},
		{"plain", "Image cannot be read", "Image cannot be read", false},
		{"empty", "", "An image in the conversation could not be processed and was removed.", false},
		{"legacy", "", "An image in the conversation could not be processed and was removed.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &lines{}
			tr := newTranslator(l)
			row := mediaRemovedRow(false, "", tc.details)
			if tc.legacy {
				var r map[string]any
				_ = json.Unmarshal(row, &r)
				delete(r, "apiError")
				delete(r, "apiErrorParams")
				delete(r, "healsDistinctCarrier")
				delete(r, "errorDetails")
				delete(r["message"].(map[string]any), "stop_reason")
				row, _ = json.Marshal(r)
			}
			tr.row(row)
			if !tr.failed || l.overed != 1 || len(l.out) != 1 {
				t.Fatalf("turn: %v, over %d", l.out, l.overed)
			}
			got := l.out[0]
			if got["type"] != "result" || got["is_error"] != true || got["api_error_status"] != float64(400) || got["result"] != tc.want || got["api_error"] != "media_removed" {
				t.Fatalf("result %v", got)
			}
			if strings.Contains(fmt.Sprint(got), "Double press") {
				t.Fatal("terminal hint leaked")
			}
			tr.row([]byte(`{"type":"system","subtype":"turn_duration"}`))
			if len(l.out) != 1 {
				t.Fatalf("second result: %v", l.out)
			}
		})
	}
}

func TestMediaRemovedThenOrdinaryError(t *testing.T) {
	l := &lines{}
	tr := newTranslator(l)
	tr.row(mediaRemovedRow(true, "tool_use", "Could not process image"))
	tr.row([]byte(`{"type":"assistant","isApiErrorMessage":true,"apiErrorStatus":429,"apiErrorParams":{"rate_limit_info":{"status":"rejected"}},"message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your limit"}]}}`))
	if got := kinds(l); !slices.Equal(got, []string{"media_removed", "assistant", "rate_limit_event", "result"}) {
		t.Fatalf("lines %v", l.out)
	}
	last := l.out[len(l.out)-1]
	if last["api_error_status"] != float64(429) || last["result"] != "You've hit your limit" || !tr.failed {
		t.Fatalf("ordinary error changed: %v", last)
	}
}

func TestMediaRemovedTerminalBeforeAnOpenTapEnds(t *testing.T) {
	l := &lines{}
	tp, write := testTap(t, l)
	tr := newTranslator(l)
	tr.tap = tp
	reply := tapReply(1, "m1")
	write(`{"tap":"on"}`, `{"s":1,"open":true}`, reply[0], reply[1], reply[2])
	tp.read()
	before := len(l.out)
	tr.row(mediaRemovedRow(false, "", "Could not process image"))
	if len(l.out) != before+1 || l.out[before]["type"] != "result" || l.out[before]["api_error_status"] != float64(400) {
		t.Fatalf("tap ended before the media failure: %v", l.out[before:])
	}
}

func TestMediaRemovedOrdinaryErrorBeforeAnOpenTapEnds(t *testing.T) {
	l := &lines{}
	tp, write := testTap(t, l)
	tr := newTranslator(l)
	tr.tap = tp
	tr.row(mediaRemovedRow(true, "tool_use", "Could not process image"))
	reply := tapReply(1, "m1")
	write(`{"tap":"on"}`, `{"s":1,"open":true}`, reply[0], reply[1], reply[2])
	tp.read()
	before := len(l.out)
	// The ordinary error must reach the gateway before settle ends the tap.
	write(`{"s":1,"end":"cut"}`)
	tr.row([]byte(`{"type":"assistant","isApiErrorMessage":true,"apiErrorStatus":429,"message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your limit"}]}}`))
	if l.out[before]["type"] != "assistant" || l.out[before]["is_api_error"] != true {
		t.Fatalf("ordinary error not identified before tap end: %v", l.out[before:])
	}
	if l.out[len(l.out)-1]["api_error_status"] != float64(429) {
		t.Fatalf("status lost: %v", l.out)
	}
}
