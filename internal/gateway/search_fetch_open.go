package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yetone/magpie/internal/provider"
)

// codexFetch reads a page by the ChatGPT backend's open, signed and
// metered as codexSearch is. All windows share a deadline independent of
// the download, and overlapping numbered lines are kept only once.
func (s *Server) codexFetch(ctx context.Context, p provider.Provider, raw string) (string, string, error) {
	ctx, cancel := context.WithTimeout(s.metered(p.Via(ctx), p, ""), webFetchTimeout/2)
	defer cancel()
	var title string
	var body strings.Builder
	total, last, chars := 0, -1, 0
	for attempt := 0; attempt < 8; attempt++ {
		output, err := s.codexOpen(ctx, p, raw, last+1)
		if err != nil {
			if last < 0 {
				return "", "", err
			}
			break
		}
		windowTitle, text, err := codexFetchText(output)
		if err == nil && fetchVerification(text) {
			err = errors.New("open returned a verification page")
		}
		if err != nil {
			if last < 0 {
				return "", "", err
			}
			break
		}
		if attempt == 0 {
			title = windowTitle
			if m := codexFetchTotal.FindStringSubmatch(output); m != nil {
				total, _ = strconv.Atoi(m[1])
			}
			if total <= 0 {
				return title, text, nil
			}
		}
		before := last
		for _, line := range strings.Split(output, "\n") {
			m := codexFetchLineNo.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			n, err := strconv.Atoi(m[1])
			// Keep a contiguous prefix so the notice never claims unseen lines.
			if err != nil || n != last+1 {
				continue
			}
			if last >= 0 {
				chars++ // the newline between collected lines
			}
			body.WriteString(line)
			body.WriteByte('\n')
			last = n
			chars += utf8.RuneCountInString(codexFetchBody(line))
			if chars > webFetchMaxChars {
				break
			}
		}
		if last >= total-1 || last == before || chars > webFetchMaxChars {
			break
		}
	}
	if last < 0 {
		return "", "", errors.New("open returned no numbered lines")
	}
	text := codexFetchBody(body.String())
	if last < total-1 {
		text += fmt.Sprintf("\n\n[the ChatGPT backend gave lines 0–%d of %d; the rest could not be read]", last, total)
	}
	return title, text, nil
}

// codexOpen asks for one window, starting near lineno (the backend may
// include preceding lines). The first request needs only the URL.
func (s *Server) codexOpen(ctx context.Context, p provider.Provider, raw string, lineno int) (string, error) {
	model := searcherModel(p)
	if model == "" {
		model = "gpt-5"
	}
	open := map[string]any{"ref_id": raw}
	if lineno > 0 {
		open["lineno"] = lineno
	}
	body, _ := json.Marshal(map[string]any{
		"id":       newUUID(),
		"model":    model,
		"commands": map[string]any{"open": []map[string]any{open}},
		"settings": map[string]any{
			"allowed_callers":     []string{"direct"},
			"external_web_access": true,
		},
		"max_output_tokens": codexSearchBudget,
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.CodexBase+AlphaSearchPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	r.Header.Set("Content-Type", "application/json")
	if err := p.Sign(ctx, r, provider.Responses, nil); err != nil {
		return "", err
	}
	if why, revoked := provider.RevokedSignIn(p.Account, r); revoked {
		return "", errors.New(why)
	}
	res, err := s.client.Do(r)
	res = tapSearch(ctx, r, body, res)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	noteSignIn(ctx, p.Account, r, res)
	b, err := io.ReadAll(io.LimitReader(res.Body, webFetchRead))
	if err != nil {
		return "", err
	}
	if res.StatusCode >= 300 {
		return "", errors.New(provider.APIError(b, res.Status))
	}
	var out codexSearchReply
	if err := json.Unmarshal(b, &out); err != nil || out.Output == nil {
		return "", errors.New("unexpected open reply")
	}
	return *out.Output, nil
}

// Drop the whole citation, including its ref_id, rather than only the
// private-use delimiters. The backend's word limit isn't page content.
var codexFetchMarks = regexp.MustCompile(`[^]*|\[wordlim: *[0-9]+\]`)

// The backend numbers each line ("L12: ", "L12@P0: ", "L262@P5-6: ") and writes a link as
// 【n†text†domain】; a reader of the page needs neither the numbers nor the ids.
var (
	codexFetchLineNo = regexp.MustCompile(`^L([0-9]+)(?:@P[0-9]+(?:-[0-9]+)?)?: ?`)
	codexFetchTotal  = regexp.MustCompile(`\bTotal lines: *([0-9]+)`)
	codexFetchLink   = regexp.MustCompile(`【[0-9]+†([^†】]*)(?:†[^】]*)?】`)
)

// codexFetchText keeps the title from "Title (URL)" and the page text,
// leaving out the backend's header and crawl/source metadata. Backend
// failures can be a successful HTTP reply whose output is only an error.
func codexFetchText(output string) (title, text string, err error) {
	output = strings.TrimSpace(strings.Map(dropPrivateUse, codexFetchMarks.ReplaceAllString(output, "")))
	head, rest, _ := strings.Cut(output, "\n")
	if i := strings.LastIndex(head, " ("); i >= 0 && strings.Contains(head[i+2:], ")") &&
		(strings.HasPrefix(head[i+2:], "http://") || strings.HasPrefix(head[i+2:], "https://")) {
		title, output = strings.TrimSpace(head[:i]), rest
	}
	text = codexFetchBody(output)
	lower := strings.ToLower(text)
	if text == "" || strings.HasPrefix(lower, "internal error") || strings.HasPrefix(lower, "failed to fetch") {
		return "", "", fmt.Errorf("open returned no page: %s", fetchClip(text, 300))
	}
	return title, text, nil
}

func codexFetchBody(output string) string {
	output = strings.Map(dropPrivateUse, codexFetchMarks.ReplaceAllString(output, ""))
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "Crawled:") || strings.HasPrefix(l, "Content type:") || strings.HasPrefix(l, "Source:") || strings.HasPrefix(l, "Total lines:") {
			continue
		}
		line = codexFetchLink.ReplaceAllString(codexFetchLineNo.ReplaceAllString(line, ""), "$1")
		if strings.TrimSpace(line) == "" {
			if len(lines) == 0 || lines[len(lines)-1] == "" {
				continue
			}
			line = ""
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
