package modal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gatewayadapter "github.com/denta-codex/codex-gateway/adapter"
)

func serveAdapterForTest(w *httptest.ResponseRecorder, r *http.Request, implementation Adapter, body []byte) {
	stream := requestStreamForTest(body)
	sink := gatewayadapter.EventSinkFunc(func(event json.RawMessage) error {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := fmt.Fprintf(w, "data: %s\n\n", event)
			return err
		}
		var terminal struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(event, &terminal) == nil && strings.HasPrefix(terminal.Type, "response.") && len(terminal.Response) > 0 && (terminal.Type == "response.completed" || terminal.Type == "response.incomplete" || terminal.Type == "response.failed") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(terminal.Response)
		}
		return nil
	})
	metadata := map[string][]string{}
	for key, values := range r.Header {
		metadata[key] = append([]string(nil), values...)
	}
	err := implementation.ServeResponses(r.Context(), gatewayadapter.Request{Body: body, Metadata: metadata}, sink)
	if err == nil {
		return
	}
	status := http.StatusBadGateway
	var public *gatewayadapter.Error
	if errors.As(err, &public) {
		status = public.Status
	}
	w.WriteHeader(status)
	_, _ = w.WriteString(err.Error())
}

func requestStreamForTest(body []byte) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	return request.Stream
}

type testSubscriptionAuth struct{}

func (testSubscriptionAuth) Token(context.Context) (string, string, error) {
	return "chatgpt-token", "chatgpt-account", nil
}

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
	serveAdapterForTest(recorder, request, adapter, body)
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
	serveAdapterForTest(recorder, request, Adapter{BaseURL: "https://modal.invalid/v1", Token: "modal-token", Client: client},
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

func TestAdapterReportsOpaqueCompactionExplicitly(t *testing.T) {
	for name, input := range map[string]string{
		"encrypted context": `[{"type":"context_compaction","encrypted_content":"opaque"}]`,
		"remote trigger":    `[{"type":"compaction_trigger"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"model":"modal/` + GLMFlashModel + `","input":` + input + `}`)
			err := (Adapter{}).ServeResponses(context.Background(), gatewayadapter.Request{Body: body}, gatewayadapter.EventSinkFunc(func(json.RawMessage) error {
				t.Fatal("compaction request emitted an event")
				return nil
			}))
			var public *gatewayadapter.Error
			if !errors.As(err, &public) || public.Code != "adapter_compaction_unsupported" || public.Status != http.StatusUnprocessableEntity {
				t.Fatalf("error=%#v", err)
			}
		})
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
		var catalog map[string]any
		if err := json.Unmarshal(model.Catalog, &catalog); err != nil || catalog["supports_search_tool"] != true || catalog["web_search_tool_type"] != "text_and_image" || catalog["prefer_websockets"] != true {
			t.Fatalf("model does not advertise hosted search: %v %#v", err, catalog)
		}
	}
	if len(seen) != len(modelSpecs) {
		t.Fatalf("got %d models, want %d", len(seen), len(modelSpecs))
	}
}

func TestAdapterRunsHostedSearchThroughSubscription(t *testing.T) {
	t.Parallel()
	modalRounds := 0
	modalClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		modalRounds++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if modalRounds == 1 {
			if !strings.Contains(string(body), `"name":"web_search"`) {
				t.Fatalf("synthetic web_search was not sent to Modal: %s", body)
			}
			sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_search","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"latest Modal news\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse)), Header: make(http.Header)}, nil
		}
		if !strings.Contains(string(body), "UNTRUSTED web content") || !strings.Contains(string(body), "https://example.com/current") {
			t.Fatalf("search result was not returned to Modal: %s", body)
		}
		sse := `data: {"choices":[{"delta":{"content":"Final answer from GLM with sources."},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse)), Header: make(http.Header)}, nil
	})}

	searchRequests := 0
	searchClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		searchRequests++
		if request.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || request.Header.Get("Authorization") != "Bearer chatgpt-token" || request.Header.Get("Chatgpt-Account-Id") != "chatgpt-account" {
			t.Fatalf("invalid ChatGPT search request: %s %#v", request.URL, request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"model":"gpt-5.6-luna"`) || !strings.Contains(string(body), `"store":false`) || !strings.Contains(string(body), `"type":"web_search"`) {
			t.Fatalf("invalid ChatGPT sidecar body: %s", body)
		}
		sse := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Current information.\\nSources:\\n- Current: https://example.com/current\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sse)), Header: http.Header{"Content-Type": []string{"text/event-stream"}}}, nil
	})}

	modalAdapter := &Adapter{BaseURL: "https://modal.invalid/v1", Token: "wk.ws", Client: modalClient, SearchClient: searchClient}
	modalAdapter.SetSubscriptionAuth(testSubscriptionAuth{})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"model":"modal/` + GLMFlashModel + `","input":"What changed?","stream":false,"tools":[{"type":"web_search","search_context_size":"high"}]}`)
	serveAdapterForTest(recorder, request, *modalAdapter, body)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Final answer from GLM") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if modalRounds != 2 || searchRequests != 1 {
		t.Fatalf("Modal rounds=%d ChatGPT searches=%d", modalRounds, searchRequests)
	}
}
