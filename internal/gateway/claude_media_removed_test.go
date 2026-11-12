package gateway

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/claudetui"
)

const mediaRemovedNotice = `{"type":"media_removed","result":"Could not process image"}`
const mediaRemovedStart = `{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1","model":"claude-sonnet-5"}}}`
const mediaRemovedText = `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a reply"}}}`
const mediaRemovedStop = `{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"}}}`
const mediaRemovedEnd = `{"type":"stream_event","event":{"type":"message_stop"}}`
const mediaRemovedResult = `{"type":"result","subtype":"success","is_error":false}`

func mediaRemovedScript(lines ...string) string {
	var s strings.Builder
	for _, line := range lines {
		s.WriteString("    echo '")
		s.WriteString(line)
		s.WriteString("'\n")
	}
	return s.String()
}

func TestMediaRemovedNoReplyIsBadRequest(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		for _, stream := range []bool{false, true} {
			for _, ending := range []string{"result", "exit", "partial", "whole partial", "legacy", "terminal", "terminal partial", "terminal whole partial", "ordinary"} {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", path, stream, ending), func(t *testing.T) {
					lines := []string{mediaRemovedNotice}
					switch ending {
					case "result":
						lines = append(lines, mediaRemovedResult)
					case "partial":
						lines = append(lines, mediaRemovedStart, mediaRemovedText, mediaRemovedEnd, mediaRemovedResult)
					case "whole partial":
						lines = append(lines, `{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-5","content":[{"type":"text","text":"a reply"}]}}`, mediaRemovedResult)
					case "legacy":
						lines = []string{`{"type":"assistant","message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: an image in the conversation could not be processed and was removed. Double press esc to edit."}]}}`, `{"type":"result","is_error":true,"result":"API Error: an image in the conversation could not be processed and was removed. Double press esc to edit."}`}
					case "terminal":
						lines = []string{`{"type":"result","is_error":true,"api_error_status":400,"api_error":"media_removed","result":"Could not process image"}`}
					case "terminal partial":
						lines = []string{mediaRemovedStart, mediaRemovedText, `{"type":"result","is_error":true,"api_error_status":400,"api_error":"media_removed","result":"Could not process image"}`}
					case "terminal whole partial":
						lines = []string{`{"type":"assistant","message":{"id":"m1","model":"claude-sonnet-5","content":[{"type":"text","text":"a reply"}]}}`, `{"type":"result","is_error":true,"api_error_status":400,"api_error":"media_removed","result":"Could not process image"}`}
					case "ordinary":
						lines = append(lines, `{"type":"result","is_error":true,"api_error_status":401,"result":"Sign-in required"}`)
					}
					script := mediaRemovedScript(lines...)
					if ending == "exit" {
						script += "    exit 0\n"
					}
					log := claudeSubscriptionFirstSays(t, script)
					s := New()
					t.Cleanup(s.subscription.abortAll)
					body := fmt.Sprintf(`{"model":"claude/claude-sonnet-5","max_tokens":50,"stream":%v,"messages":[{"role":"user","content":"look"}]}`, stream)
					code, reply := sendTo(s, path, body)
					runs, err := os.ReadFile(log)
					if err != nil {
						t.Fatal(err)
					}
					if ending == "ordinary" {
						if code != 200 || !strings.Contains(reply, "pong") || len(strings.Fields(string(runs))) != 2 {
							t.Fatalf("ordinary failure no longer falls back: %d %s (%s)", code, reply, runs)
						}
						return
					}
					want := "Could not process image"
					if ending == "legacy" {
						want = claudetui.MediaRemovedMessage("")
					}
					if stream && (ending == "partial" || ending == "whole partial" || ending == "terminal partial" || ending == "terminal whole partial") {
						if code != 200 || !strings.Contains(reply, `"type":"invalid_request_error"`) || !strings.Contains(reply, `"error":`) || !strings.Contains(reply, want) || strings.Contains(reply, "event: message_stop") || strings.Contains(reply, "[DONE]") {
							t.Fatalf("stream: %d %s", code, reply)
						}
						if path == "/v1/messages" && !strings.Contains(reply, "event: error") {
							t.Fatalf("not an Anthropic error event: %s", reply)
						}
					} else if code != 400 || !strings.Contains(reply, `"type":"invalid_request_error"`) || !strings.Contains(reply, want) {
						t.Fatalf("response: %d %s", code, reply)
					}
					if strings.Contains(reply, "Double press") || strings.Contains(reply, "pong") {
						t.Fatalf("hint or spare answer leaked: %s", reply)
					}
					if string(runs) != "own\n" {
						t.Fatalf("another account was tried: %q", runs)
					}
					route := lastRoute(s)
					if len(route.Tries) != 1 || route.Tries[0].Rest != nil {
						t.Fatalf("tries/rest: %+v", route.Tries)
					}
				})
			}
		}
	}
}

func TestMediaRemovedReplyInEitherOrder(t *testing.T) {
	for _, first := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream-first=%v/stream=%v", first, stream), func(t *testing.T) {
				lines := []string{mediaRemovedNotice, mediaRemovedStart, mediaRemovedText}
				if first {
					lines = []string{mediaRemovedStart, mediaRemovedText, mediaRemovedNotice}
				}
				lines = append(lines, mediaRemovedStop, mediaRemovedEnd, mediaRemovedResult)
				log := claudeSubscriptionFirstSays(t, mediaRemovedScript(lines...))
				s := New()
				t.Cleanup(s.subscription.abortAll)
				code, reply := sendTo(s, "/v1/messages", fmt.Sprintf(`{"model":"claude/claude-sonnet-5","max_tokens":50,"stream":%v,"messages":[{"role":"user","content":"look"}]}`, stream))
				if code != 200 || !strings.Contains(reply, "a reply") || strings.Contains(reply, "Could not process") || strings.Contains(reply, `"error":`) {
					t.Fatalf("%d %s", code, reply)
				}
				runs, _ := os.ReadFile(log)
				if string(runs) != "own\n" {
					t.Fatalf("runs %q", runs)
				}
			})
		}
	}
}

func TestMediaRemovedIsNotRetryableOrRested(t *testing.T) {
	for _, message := range []string{
		"Could not process image",
		"Image was specified using the image/png media type, but the image appears to be a image/webp image",
		"Image was specified using the image/png media type, but the image appears to be a image/jpeg image",
		claudetui.MediaRemovedMessage(""),
	} {
		body := mustJSON(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": message}})
		if retryable(http.StatusBadRequest, body) {
			t.Fatalf("retryable: %s", message)
		}
		if why := failure(http.StatusBadRequest, body); why != failOther {
			t.Fatalf("failure %s: %s", why, message)
		}
		// relay carries this status on the try even after HTTP 200 went out.
		header := http.Header{}
		header.Set(namedStatusHeader, "400")
		if named := failedNamed(header, 200); named != 400 || retryable(named, body) {
			t.Fatalf("stream error can rest: %d %s", named, message)
		}
	}
}

func TestMediaRemovedOrdinaryFailureAfterPartialReply(t *testing.T) {
	run := &subscriptionRun{}
	segment := run.attach()
	done := make(chan struct{})
	lines := []string{mediaRemovedNotice, mediaRemovedStart, mediaRemovedText,
		`{"type":"assistant","is_api_error":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"Server unavailable"}]}}`,
		mediaRemovedEnd, `{"type":"result","is_error":true,"api_error_status":503,"result":"Server unavailable"}`}
	go func() { defer close(done); run.readOutput(strings.NewReader(strings.Join(lines, "\n") + "\n")) }()
	var failure Event
	for e := range segment {
		if e.Kind == KError {
			failure = e
		}
	}
	<-done
	if failure.Status != 502 || !strings.Contains(failure.Text, "stream from Anthropic was cut short") {
		t.Fatalf("ordinary cut changed: %+v", failure)
	}
}
