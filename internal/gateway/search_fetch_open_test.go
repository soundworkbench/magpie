package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func fetchBackend(t *testing.T, handle http.HandlerFunc) {
	t.Helper()
	up := httptest.NewServer(handle)
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	if err := catalog.SaveLive("codex", provider.CodexBase, []catalog.Model{{ID: "gpt-6-luna"}}); err != nil {
		t.Fatal(err)
	}
}

func fetchSearcher(t *testing.T, named string) {
	t.Helper()
	st := settings.Load()
	st.Searcher = named
	if err := settings.Save(st); err != nil {
		t.Fatal(err)
	}
}

// Successful downloads never spend a backend call, even with @search selected.
func TestWebFetchDownloadBeforeOpen(t *testing.T) {
	codexSignedIn(t)
	var downloaded, opened atomic.Int32
	fetchBackend(t, func(w http.ResponseWriter, r *http.Request) {
		opened.Add(1)
		http.Error(w, "unexpected open", 500)
	})
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloaded.Add(1)
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<title>Local page</title><p>Downloaded text</p>")
	}))
	t.Cleanup(page.Close)
	useFetchClient(t, page.Client())
	fetchSearcher(t, "codex/@search")
	h := lanGuard(New().Handler())
	got, isErr := fetchText_(t, h, fetchCall(page.URL, ""))
	if want := "URL: " + page.URL + "\nTitle: Local page\n\nDownloaded text"; isErr || got != want {
		t.Fatalf("got %q, error %v, want %q", got, isErr, want)
	}
	for _, call := range []string{fetchCall("file:///etc/passwd", ""), fetchCall(page.URL, `,"offset":-1`)} {
		if got, isErr := fetchText_(t, h, call); !isErr {
			t.Fatalf("invalid arguments accepted: %q", got)
		}
	}
	if downloaded.Load() != 1 || opened.Load() != 0 {
		t.Fatalf("downloaded %d, opened %d", downloaded.Load(), opened.Load())
	}
}

func useFetchClient(t *testing.T, client *http.Client) {
	t.Helper()
	was := webFetchClient
	webFetchClient = client
	t.Cleanup(func() { webFetchClient = was })
}

// A refused download and a PDF both fall back to open. Each numbered window
// overlaps the preceding one; pagination must reach text from the last window.
func TestWebFetchOpenReadsOverlappingWindows(t *testing.T) {
	for _, kind := range []string{"403", "PDF"} {
		t.Run(kind, func(t *testing.T) {
			codexSignedIn(t)
			var downloaded, opened atomic.Int32
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloaded.Add(1)
				if kind == "403" {
					http.Error(w, "denied", 403)
				} else {
					w.Header().Set("Content-Type", "application/pdf")
					io.WriteString(w, "%PDF-1.7")
				}
			}))
			t.Cleanup(page.Close)
			useFetchClient(t, page.Client())
			fetchBackend(t, func(w http.ResponseWriter, r *http.Request) {
				call := opened.Add(1)
				var q struct {
					ID       string `json:"id"`
					Model    string `json:"model"`
					Commands struct {
						Open []struct {
							RefID  string `json:"ref_id"`
							Lineno *int   `json:"lineno"`
						} `json:"open"`
						Search json.RawMessage `json:"search_query"`
					} `json:"commands"`
					Settings struct {
						Allowed []string `json:"allowed_callers"`
						Live    bool     `json:"external_web_access"`
					} `json:"settings"`
				}
				if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
					t.Error(err)
				}
				if downloaded.Load() == 0 || r.Method != http.MethodPost ||
					r.URL.Path != "/backend-api/codex/alpha/search" || r.Header.Get("Authorization") == "" ||
					r.Header.Get("chatgpt-account-id") != "acct-1" || len(q.ID) != 36 ||
					q.Model != "gpt-6-luna" || len(q.Commands.Open) != 1 || len(q.Commands.Search) != 0 ||
					!q.Settings.Live || strings.Join(q.Settings.Allowed, ",") != "direct" {
					t.Errorf("unexpected open request: %s %s, %+v", r.Method, r.URL.Path, q)
				}
				if len(q.Commands.Open) != 1 {
					w.WriteHeader(400)
					return
				}
				open := q.Commands.Open[0]
				if open.RefID != page.URL {
					t.Errorf("ref_id = %q", open.RefID)
				}
				line := 0
				if open.Lineno != nil {
					line = *open.Lineno
				}
				if want := []int{0, 3, 5}[(call-1)%3]; line != want || (want == 0 && open.Lineno != nil) {
					t.Errorf("call %d: lineno = %v, want %d", call, open.Lineno, want)
				}
				windows := map[int]string{
					0: "L0: 你好\nL1@P0: 世界\nL2@P5-6: abcde",
					3: "L1@P0: 世界\nL2@P5-6: abcde\nL3: 倒数\nL4: 第二",
					5: "L3: 倒数\nL4: 第二\nL5@P6: 【4†结束†example.com】",
				}
				json.NewEncoder(w).Encode(map[string]any{"output": "页面 (https://example.com/final)\nciteturn0view0 [wordlim: 200] Content type: application/pdf; Source: open({}); Total lines: 6\n" + windows[line]})
			})
			fetchSearcher(t, "codex/@search")
			h := lanGuard(New().Handler())
			for _, c := range []struct{ args, text string }{
				{"", "你好\n世界\nabcde\n倒数\n第二\n结束"},
				{`,"offset":18`, "结束"},
				{`,"max_chars":2,"offset":18`, "结束"},
			} {
				got, isErr := fetchText_(t, h, fetchCall(page.URL, c.args))
				if want := "URL: " + page.URL + "\nTitle: 页面\n\n" + c.text; isErr || got != want {
					t.Fatalf("%s: got %q, error %v, want %q", c.args, got, isErr, want)
				}
			}
			if downloaded.Load() != 3 || opened.Load() != 9 {
				t.Fatalf("downloaded %d, opened %d", downloaded.Load(), opened.Load())
			}
		})
	}
}

// Failed backend reads preserve the download error, including HTTP 200
// backend replies that contain only an error rather than page content.
func TestWebFetchOpenFailureKeepsDownloadError(t *testing.T) {
	for _, c := range []struct {
		name, reply string
		status      int
	}{
		{"internal", `{"output":"Internal Error"}`, 200},
		{"payment", `{"output":"Failed to fetch https://example.com/: (402) Payment Required"}`, 200},
		{"timeout message", `{"output":"Failed to fetch https://example.com/: (400) Timeout fetching"}`, 200},
		{"header then error", `{"output":"Page (https://example.com/)\nciteturn0view0 [wordlim: 200] Source: open({});\nInternal Error"}`, 200},
		{"empty", `{"output":""}`, 200},
		{"missing output", `{}`, 200},
		{"malformed", `{"output":`, 200},
		{"http error", `{"error":{"message":"unavailable"}}`, 503},
		{"verification", `{"output":"please click the box below to let us know you're not a robot"}`, 200},
	} {
		t.Run(c.name, func(t *testing.T) {
			codexSignedIn(t)
			var opened atomic.Int32
			fetchBackend(t, func(w http.ResponseWriter, r *http.Request) {
				opened.Add(1)
				w.WriteHeader(c.status)
				io.WriteString(w, c.reply)
			})
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "download denied", 403)
			}))
			t.Cleanup(page.Close)
			useFetchClient(t, page.Client())
			fetchSearcher(t, "codex/@search")
			got, isErr := fetchText_(t, lanGuard(New().Handler()), fetchCall(page.URL, ""))
			want := "couldn't fetch " + page.URL + ": the site answered 403 Forbidden: download denied"
			if !isErr || got != want || opened.Load() != 1 {
				t.Fatalf("got %q, error %v, opens %d; want %q", got, isErr, opened.Load(), want)
			}
		})
	}
}

func TestWebFetchVerification(t *testing.T) {
	for _, c := range []struct {
		name, backend string
		fail          bool
	}{
		{"backend reads content", "Page (https://example.com/)\nReadable content", false},
		{"both challenge", "L0@P0: Please VERIFY YOU ARE HUMAN", true},
		{"backend fails", "Internal Error", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			codexSignedIn(t)
			var downloaded, opened atomic.Int32
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloaded.Add(1)
				w.Header().Set("Content-Type", "text/html")
				io.WriteString(w, "<h2>环境异常</h2><p>当前环境异常，完成验证后即可继续访问。</p><a>去验证</a>")
			}))
			t.Cleanup(page.Close)
			useFetchClient(t, page.Client())
			fetchBackend(t, func(w http.ResponseWriter, r *http.Request) {
				opened.Add(1)
				if downloaded.Load() != 1 {
					t.Error("open ran before the download")
				}
				json.NewEncoder(w).Encode(map[string]any{"output": c.backend})
			})
			fetchSearcher(t, "codex/@search")
			got, isErr := fetchText_(t, lanGuard(New().Handler()), fetchCall(page.URL, ""))
			want := "URL: " + page.URL + "\nTitle: Page\n\nReadable content"
			if c.fail {
				want = "couldn't read " + page.URL + ": the site showed a verification page instead of the content"
			}
			if got != want || isErr != c.fail || opened.Load() != 1 {
				t.Fatalf("got %q, error %v, opens %d; want %q, error %v", got, isErr, opened.Load(), want, c.fail)
			}
		})
	}
}

// A download that comes back empty (a redirect to a captcha page) is not
// the page either: open gets to try.
func TestWebFetchEmptyDownloadOpens(t *testing.T) {
	codexSignedIn(t)
	var opened atomic.Int32
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body><script>verify()</script></body></html>")
	}))
	t.Cleanup(page.Close)
	useFetchClient(t, page.Client())
	fetchBackend(t, func(w http.ResponseWriter, r *http.Request) {
		opened.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"output": "Internal Error"})
	})
	fetchSearcher(t, "codex/@search")
	got, isErr := fetchText_(t, lanGuard(New().Handler()), fetchCall(page.URL, ""))
	want := "couldn't read " + page.URL + ": the page had no text"
	if got != want || !isErr || opened.Load() != 1 {
		t.Fatalf("got %q, error %v, opens %d; want %q", got, isErr, opened.Load(), want)
	}
}

// Having an account available is not consent to use backend open.
func TestWebFetchOpenOnlyWhenSelected(t *testing.T) {
	codexSignedIn(t)
	var opened atomic.Int32
	fetchBackend(t, func(w http.ResponseWriter, r *http.Request) {
		opened.Add(1)
		http.Error(w, "unexpected open", 500)
	})
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "download denied", 403)
	}))
	t.Cleanup(page.Close)
	useFetchClient(t, page.Client())
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Key: "test-key", Searches: true, Responses: page.URL}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("relay", page.URL, []catalog.Model{{ID: "gpt-6-luna"}}); err != nil {
		t.Fatal(err)
	}
	h := lanGuard(New().Handler())
	for _, named := range []string{"", "off", "codex", "codex/gpt-6-luna", "missing/@search", "relay/@search"} {
		fetchSearcher(t, named)
		got, isErr := fetchText_(t, h, fetchCall(page.URL, ""))
		if !isErr || !strings.Contains(got, "403 Forbidden: download denied") {
			t.Fatalf("%q: %q, error %v", named, got, isErr)
		}
	}
	if opened.Load() != 0 {
		t.Fatalf("opened %d times without the explicit choice", opened.Load())
	}
}

// An incomplete read is visible, whether the backend stops making progress,
// fails during continuation, or reaches either of the bounded-read limits.
func TestWebFetchOpenIncomplete(t *testing.T) {
	for _, c := range []struct {
		name        string
		calls, last int
		offset      int
	}{
		{"request limit", 8, 15, 0},
		{"no progress", 2, 1, 0},
		{"continuation error", 2, 1, 0},
		{"continuation challenge", 2, 1, 0},
		{"character limit", 2, 3, 100003},
	} {
		t.Run(c.name, func(t *testing.T) {
			codexSignedIn(t)
			var opened atomic.Int32
			page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "denied", 403)
			}))
			t.Cleanup(page.Close)
			useFetchClient(t, page.Client())
			fetchBackend(t, func(w http.ResponseWriter, r *http.Request) {
				call := opened.Add(1)
				var q struct {
					Commands struct {
						Open []struct {
							Lineno int `json:"lineno"`
						} `json:"open"`
					} `json:"commands"`
				}
				if err := json.NewDecoder(r.Body).Decode(&q); err != nil || len(q.Commands.Open) != 1 {
					t.Errorf("unexpected open: %+v, error %v", q, err)
					w.WriteHeader(400)
					return
				}
				if call > 1 && c.name == "continuation error" {
					http.Error(w, "unavailable", 503)
					return
				}
				if call > 1 && c.name == "continuation challenge" {
					json.NewEncoder(w).Encode(map[string]any{"output": "Just a moment..."})
					return
				}
				line := q.Commands.Open[0].Lineno
				if c.name == "no progress" {
					line = 0
				}
				var output strings.Builder
				output.WriteString("Page (https://example.com/)\nTotal lines: 20\n")
				for n := max(0, line-1); n < line+2; n++ {
					text := fmt.Sprintf("line %d", n)
					if c.name == "character limit" {
						text = strings.Repeat("文", 25000)
					}
					fmt.Fprintf(&output, "L%d@P0: %s\n", n, text)
				}
				json.NewEncoder(w).Encode(map[string]any{"output": output.String()})
			})
			fetchSearcher(t, "codex/@search")
			got, isErr := fetchText_(t, lanGuard(New().Handler()), fetchCall(page.URL, fmt.Sprintf(`,"max_chars":100000,"offset":%d`, c.offset)))
			note := fmt.Sprintf("[the ChatGPT backend gave lines 0–%d of 20; the rest could not be read]", c.last)
			if isErr || !strings.HasSuffix(got, note) || opened.Load() != int32(c.calls) {
				t.Fatalf("got %q, error %v, opens %d; want suffix %q, opens %d", got, isErr, opened.Load(), note, c.calls)
			}
			if c.offset == 0 && (!strings.Contains(got, "\nline 0\nline 1\n") || strings.Contains(got, "Just a moment")) {
				t.Fatalf("lost initial content or returned challenge: %q", got)
			}
		})
	}
}

func TestFetchVerification(t *testing.T) {
	for _, marker := range []string{
		"环境异常", "完成验证后即可继续访问", "not a robot", "verify you are human",
		"Just a moment...", "Attention Required! | Cloudflare",
	} {
		t.Run(marker, func(t *testing.T) {
			if !fetchVerification(" \n" + strings.ToUpper(marker) + "\t ") {
				t.Fatal("missed a verification page")
			}
			compact := strings.Join(strings.Fields(marker), "")
			boundary := compact + strings.Repeat("文", 1500-len([]rune(compact)))
			if !fetchVerification(boundary) || fetchVerification(boundary+"文") {
				t.Fatal("verification classifier did not honor the 1500-character boundary")
			}
		})
	}
	if fetchVerification("A normal page.") || fetchVerification("") {
		t.Fatal("ordinary content was classified as verification")
	}
}

func TestCodexFetchText(t *testing.T) {
	for _, c := range []struct{ output, title, text string }{
		{"plain page text", "", "plain page text"},
		{"Page (notes) (https://example.com/)\nciteturn0view0 [wordlim: 200] Crawled: today; Content type: text/html; Source: open({});\nFirst paragraph.\n\nSecond paragraph.", "Page (notes)", "First paragraph.\n\nSecond paragraph."},
		{"Page (https://example.com/)\nContent type: text/html\nSource: open({});\nBody", "Page", "Body"},
		{"Source: notes (https://example.com/) citeturn0view0 [wordlim: 200] Crawled: today;\nBody", "Source: notes", "Body"},
	} {
		title, text, err := codexFetchText(c.output)
		if err != nil || title != c.title || text != c.text {
			t.Fatalf("%q: title %q, text %q, error %v", c.output, title, text, err)
		}
	}
}

func TestCodexFetchTextDropsLineNumbersAndLinkIDs(t *testing.T) {
	out := "Example (https://example.com/)\nciteturn0view0 [wordlim: 200] Crawled: today; Content type: text/html; Source: open({\"ref_id\":\"https://example.com/\"});\nTotal lines: 4\nL0: 【0†Jump to content】\nL1@P0: \nL2: \nL3@P5-6: See 【4†Docs†example.com】 now"
	title, text, err := codexFetchText(out)
	if err != nil || title != "Example" || text != "Jump to content\n\nSee Docs now" {
		t.Fatalf("title %q text %q err %v", title, text, err)
	}
}
