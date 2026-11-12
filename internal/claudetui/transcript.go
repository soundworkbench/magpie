package claudetui

import (
	"encoding/json"
	"log"
	"strings"
)

// emitter is where a translator's lines go.
type emitter interface {
	emit(v any)
	// a message the user sent reached the transcript, and a turn ended
	sawRow()
	turnOver()
}

// translator tells the bridge, as `claude -p`'s stream-json, what the
// session's transcript says. Claude Code writes a row a content block of an
// assistant message, under the message's id, as -p tells one not streamed:
// each goes on as it is. An API error is a <synthetic> message whose text
// is the error, and a turn's end a turn_duration row: they become the
// turn's result, as -p ends a turn.
type translator struct {
	out emitter
	// the turn failed: its <synthetic> row's result is told, and the
	// turn_duration after it says nothing more
	failed bool
	// tap has the streams of the messages (tap.go): a message is told
	// after its stream has begun, and a turn's end after its streams'
	tap *tap
}

func newTranslator(out emitter) *translator { return &translator{out: out} }

// quiet and flush are for a translator that holds rows back; this one
// tells each as it comes.
func (t *translator) quiet() {}
func (t *translator) flush() {}

func (t *translator) row(b []byte) {
	var r struct {
		Type          string          `json:"type"`
		Subtype       string          `json:"subtype"`
		UUID          string          `json:"uuid"`
		IsSidechain   bool            `json:"isSidechain"`
		IsMeta        bool            `json:"isMeta"`
		RequestID     string          `json:"requestId"`
		Error         json.RawMessage `json:"error"`
		IsAPIError    bool            `json:"isApiErrorMessage"`
		APIError      string          `json:"apiError"`
		ErrorDetails  string          `json:"errorDetails"`
		Heals         bool            `json:"healsDistinctCarrier"`
		APIStatus     int             `json:"apiErrorStatus"`
		ToolUseResult json.RawMessage `json:"toolUseResult"`
		Operation     string          `json:"operation"`
		APIParams     struct {
			Media         string          `json:"media"`
			MediaReason   string          `json:"media_reason"`
			RateLimitInfo json.RawMessage `json:"rate_limit_info"`
		} `json:"apiErrorParams"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(b, &r) != nil || r.IsSidechain {
		return
	}
	switch r.Type {
	case "queue-operation":
		// a message sent mid-turn waits in Claude Code's queue
		if r.Operation == "enqueue" {
			t.out.sawRow()
		}
	case "user":
		if !r.IsMeta && isPrompt(r.Message) {
			t.failed = false
			t.out.sawRow()
		}
		if len(r.ToolUseResult) > 0 && r.ToolUseResult[0] == '{' {
			t.out.emit(map[string]any{"type": "user", "tool_use_result": r.ToolUseResult})
		}
	case "assistant":
		var m struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Stop    string `json:"stop_reason"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(r.Message, &m)
		text := ""
		for _, c := range m.Content {
			if c.Type == "text" {
				text += c.Text
			}
		}
		if m.Model == "<synthetic>" && (r.APIError == "media_removed" || strings.Contains(strings.ToLower(text), MediaRemovedSentence)) {
			// Claude Code already asked again without the image. Settling
			// here would cut that request's stream if it began first.
			message := MediaRemovedMessage(r.ErrorDetails)
			log.Printf("claude-tui: media_removed: media=%q reason=%q continues=%t message_bytes=%d", r.APIParams.Media[:min(len(r.APIParams.Media), 64)], r.APIParams.MediaReason[:min(len(r.APIParams.MediaReason), 64)], r.Heals || m.Stop == "tool_use", len(message))
			if r.Heals || m.Stop == "tool_use" {
				t.out.emit(map[string]any{"type": "media_removed", "result": message})
				return
			}
			// Tell the failure before a still-open tap can end as a cut
			// stream. The turn_duration row settles it afterwards.
			t.failed = true
			t.out.turnOver()
			t.out.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
				"result": message, "api_error_status": 400, "api_error": "media_removed"})
			return
		}
		if m.Model != "<synthetic>" && m.ID != "" {
			t.tap.before(m.ID)
		}
		line := map[string]any{"type": "assistant", "message": r.Message}
		// the row's place in the session, as -p tells it: the bridge
		// branches a conversation off there (cutSession)
		if r.UUID != "" {
			line["uuid"] = r.UUID
		}
		if r.RequestID != "" {
			line["request_id"] = r.RequestID
		}
		if len(r.Error) > 0 {
			line["error"] = r.Error
		}
		if r.IsAPIError {
			line["is_api_error"] = true
		}
		t.out.emit(line)
		if m.Model != "<synthetic>" || !r.IsAPIError {
			return
		}
		t.failed = true
		// the error's own allowance after any an earlier reply of the
		// turn told (tap.settle), as it is the latest
		t.tap.settle()
		if len(r.APIParams.RateLimitInfo) > 0 && string(r.APIParams.RateLimitInfo) != "null" {
			t.out.emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": r.APIParams.RateLimitInfo})
		}
		t.out.turnOver()
		t.out.emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true,
			"result": text, "api_error_status": r.APIStatus})
	case "system":
		if r.Subtype == "turn_duration" {
			t.tap.settle()
		}
		if r.Subtype == "turn_duration" && !t.failed {
			t.out.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false})
		}
		if r.Subtype == "turn_duration" {
			t.failed = false
			t.out.turnOver()
		}
	}
}

// isPrompt says whether a user row's message is one typed in: text, not a
// tool's result.
func isPrompt(raw json.RawMessage) bool {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil || len(m.Content) == 0 {
		return false
	}
	if m.Content[0] == '"' {
		return true
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return false
		}
	}
	return len(blocks) > 0
}

// MediaRemovedSentence is the fixed part of Claude Code's older -p error,
// whose row has none of the interactive mode's API error fields.
const MediaRemovedSentence = "an image in the conversation could not be processed and was removed"

// MediaRemovedMessage keeps Anthropic's words, without Claude Code's
// terminal editing hint. A missing detail gets a sentence of its own.
func MediaRemovedMessage(details string) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(details), &body) == nil && body.Error.Message != "" {
		return body.Error.Message
	}
	if strings.TrimSpace(details) != "" {
		return details
	}
	return "An image in the conversation could not be processed and was removed."
}
