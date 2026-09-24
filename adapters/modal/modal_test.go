package modal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestAdapterEndToEndAndRetry(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	attempts := 0
	sessionID := ""
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if request.Header.Get("Authorization") != "Bearer wk.ws" {
			t.Errorf("unexpected authorization header")
		}
		currentSessionID := request.Header.Get("Modal-Session-Id")
		if !strings.HasPrefix(currentSessionID, "codex-") {
			t.Errorf("missing stable Modal session ID")
		}
		if sessionID == "" {
			sessionID = currentSessionID
		} else if currentSessionID != sessionID {
			t.Errorf("retry changed Modal session ID")
		}
		var chat map[string]any
		if err := json.NewDecoder(request.Body).Decode(&chat); err != nil {
			t.Error(err)
		}
		if chat["model"] != GLMFlashModel || chat["stream"] != true {
			t.Errorf("unexpected upstream body: %#v", chat)
		}
		if attempts == 1 {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("rate limited")), Header: make(http.Header)}, nil
		}
		sse := `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse)), Header: make(http.Header)}, nil
	})}
	adapter := Adapter{BaseURL: "https://modal.invalid/v1", Token: "wk.ws", Client: client}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("session-id", "thread-1")
	body := []byte(`{"model":"modal/` + GLMFlashModel + `","input":"hello","stream":false}`)
	adapter.ServeResponses(recorder, request, body)
	if attempts != 2 {
		t.Fatalf("got %d attempts, want 2", attempts)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response["status"] != "completed" {
		t.Fatalf("invalid Responses result: %v %#v", err, response)
	}
}

func TestAdapterDoesNotForwardCallerCredentials(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer modal-token" || request.Header.Get("Cookie") != "" || request.Header.Get("X-Secret") != "" {
			t.Fatalf("caller credentials leaked upstream: %#v", request.Header)
		}
		sse := `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse)), Header: make(http.Header)}, nil
	})}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer caller-secret")
	request.Header.Set("Cookie", "private=true")
	request.Header.Set("X-Secret", "private")
	Adapter{BaseURL: "https://modal.invalid/v1", Token: "modal-token", Client: client}.ServeResponses(recorder, request,
		[]byte(`{"model":"modal/`+GLMFlashModel+`","input":"hello","stream":true}`))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestEndpointRequiresHTTPSOutsideLoopback(t *testing.T) {
	t.Parallel()
	if _, err := (Adapter{BaseURL: "http://modal.example/v1"}).endpoint(); err == nil {
		t.Fatal("accepted insecure Modal origin")
	}
	if got, err := (Adapter{BaseURL: "http://127.0.0.1:1234/v1"}).endpoint(); err != nil || got != "http://127.0.0.1:1234/v1/chat/completions" {
		t.Fatalf("loopback test endpoint = %q, %v", got, err)
	}
}

func TestModelsAreNamespacedAndUnique(t *testing.T) {
	t.Parallel()
	if len(modelSpecs) != 3 {
		t.Fatalf("got %d Modal models, want the three approved endpoints", len(modelSpecs))
	}
	seen := map[string]bool{}
	for _, model := range (Adapter{}).Models() {
		if !strings.HasPrefix(model.Slug, Namespace+"/") || seen[model.Slug] || !json.Valid(model.Catalog) {
			t.Fatalf("invalid model entry: %#v", model)
		}
		seen[model.Slug] = true
	}
	if len(seen) != len(modelSpecs) {
		t.Fatalf("got %d models, want %d", len(seen), len(modelSpecs))
	}
}
