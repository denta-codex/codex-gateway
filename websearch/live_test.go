package websearch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denta-codex/codex-gateway/internal/subscription"
)

const liveGLMFlashModel = "andrew-61005--ep-codex-tasks-shared-glm-server.us-west.modal.direct"

type liveSearchRecorder struct {
	t      *testing.T
	mu     sync.Mutex
	sends  int
	failed bool
	events map[string]bool
	base   http.RoundTripper
}

type timedLiveBody struct {
	io.ReadCloser
	t     *testing.T
	label string
	start time.Time
	bytes int
	once  sync.Once
}

func (b *timedLiveBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	b.bytes += n
	if err != nil {
		b.report(err)
	}
	return n, err
}

func (b *timedLiveBody) Close() error {
	b.report(nil)
	return b.ReadCloser.Close()
}

func (b *timedLiveBody) report(err error) {
	b.once.Do(func() {
		b.t.Logf("%s completed in %s with %d response bytes (terminal=%v)", b.label, time.Since(b.start).Round(time.Millisecond), b.bytes, err)
	})
}

func (r *liveSearchRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	_, parseErr := parseSearchResponse(&http.Response{StatusCode: response.StatusCode, Header: response.Header.Clone(), Body: io.NopCloser(bytes.NewReader(body))})
	events := map[string]bool{}
	var failureCode string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			continue
		}
		kind, _ := event["type"].(string)
		events[kind] = true
		if kind == "response.failed" || kind == "error" {
			failureCode = liveFailureCode(event)
		}
	}
	r.mu.Lock()
	r.sends++
	if failureCode != "" || parseErr != nil {
		r.failed = true
	}
	if r.events == nil {
		r.events = map[string]bool{}
	}
	for event := range events {
		r.events[event] = true
	}
	r.mu.Unlock()
	r.t.Logf("ChatGPT sidecar HTTP %d content-type=%q bytes=%d events=%v failure=%q parse_error=%v", response.StatusCode, response.Header.Get("Content-Type"), len(body), events, failureCode, parseErr)
	return response, nil
}

func liveFailureCode(value any) string {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if code := liveFailureCode(item); code != "" {
				return code
			}
		}
	case map[string]any:
		if code, _ := typed["code"].(string); code != "" {
			return code
		}
		if kind, _ := typed["type"].(string); kind != "" && kind != "response.failed" && kind != "error" {
			return kind
		}
		for _, item := range typed {
			if code := liveFailureCode(item); code != "" {
				return code
			}
		}
	}
	return ""
}

// TestLiveGraceModalSearch uses the installed Grace gateway only as the Modal
// Responses backend. The web-search loop and ChatGPT sidecar are the code under
// test from this worktree, so no service deployment or configuration change is
// required. Enable explicitly because it consumes live subscription and Modal
// inference capacity.
func TestLiveGraceModalSearch(t *testing.T) {
	if os.Getenv("CODEX_GATEWAY_LIVE_SEARCH") != "1" {
		t.Skip("set CODEX_GATEWAY_LIVE_SEARCH=1 to run the live Grace probe")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	auth := &subscription.Auth{Path: filepath.Join(home, ".codex", "auth.json"), CodexBinary: "codex"}
	searchRecorder := &liveSearchRecorder{t: t, base: http.DefaultTransport}
	service, err := New(Config{Auth: auth, Client: &http.Client{Transport: searchRecorder}, MaxSearches: 1})
	if err != nil {
		t.Fatal(err)
	}
	backendClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	modalRound := 0
	backend := BackendFunc(func(ctx context.Context, body []byte) (*http.Response, error) {
		modalRound++
		started := time.Now()
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:48766/v1/responses", bytes.NewReader(body))
		if requestErr != nil {
			return nil, requestErr
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Thread-Id", "codex-gateway-live-web-search")
		response, responseErr := backendClient.Do(request)
		if responseErr == nil {
			response.Body = &timedLiveBody{ReadCloser: response.Body, t: t, label: "Modal round " + strconv.Itoa(modalRound), start: started}
		}
		return response, responseErr
	})
	body := []byte(`{"model":"modal/` + liveGLMFlashModel + `","stream":true,"input":"Use web_search to find the newest stable Go release from official sources. Answer concisely and include the source URLs.","tools":[{"type":"web_search","filters":{"allowed_domains":["go.dev"]},"search_context_size":"high"}],"tool_choice":{"type":"web_search"}}`)
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	service.ServeResponses(recorder, request, body, backend)
	result, err := io.ReadAll(recorder.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	searchRecorder.mu.Lock()
	searchFailed, searchSends := searchRecorder.failed, searchRecorder.sends
	searchRecorder.mu.Unlock()
	lowerResult := strings.ToLower(string(result))
	if recorder.Code != http.StatusOK || !bytes.Contains(result, []byte("response.completed")) || !strings.Contains(string(result), "http") || searchFailed || searchSends < 1 || searchSends > maxQueries || strings.Contains(lowerResult, "search tool is currently") || strings.Contains(lowerResult, "search tool failure") {
		t.Fatalf("live search status=%d body=%s", recorder.Code, result)
	}
	t.Logf("live Modal + ChatGPT search succeeded: Modal rounds=%d ChatGPT searches=%d response bytes=%d", modalRound, searchSends, len(result))
}
