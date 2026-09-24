package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAuth struct {
	mu       sync.Mutex
	token    string
	account  string
	err      error
	refresh  int
	newToken string
}

func (a *fakeAuth) Token(context.Context) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token, a.account, a.err
}

func (a *fakeAuth) RefreshIfUnchanged(_ context.Context, rejected string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == rejected {
		a.token = a.newToken
	}
	a.refresh++
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func httpResponse(status int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func serviceFor(t *testing.T, auth *fakeAuth, transport roundTripFunc, configure func(*Config)) *Service {
	t.Helper()
	config := Config{Auth: auth, Client: &http.Client{Transport: transport}}
	if configure != nil {
		configure(&config)
	}
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func requestBody(stream bool) []byte {
	data, _ := json.Marshal(map[string]any{
		"model":  "modal/glm-5.3-flash",
		"stream": stream,
		"input":  []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "What changed today?"}}}},
		"tools":  []any{map[string]any{"type": "web_search", "search_context_size": "high", "filters": map[string]any{"allowed_domains": []any{"example.com"}}}},
	})
	return data
}

func searchCallResponse(callID, query string, extra ...map[string]any) *http.Response {
	output := []any{map[string]any{"type": "function_call", "name": "web_search", "call_id": callID, "arguments": `{"query":` + strconvQuote(query) + `}`}}
	for _, item := range extra {
		output = append(output, item)
	}
	body, _ := json.Marshal(map[string]any{"id": "round", "object": "response", "status": "completed", "output": output})
	return httpResponse(http.StatusOK, "application/json", string(body))
}

func finalResponse(text string) *http.Response {
	body, _ := json.Marshal(map[string]any{"id": "final", "object": "response", "status": "completed", "output": []any{
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}},
	}})
	return httpResponse(http.StatusOK, "application/json", string(body))
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestSearchLoopUsesChatGPTHostedToolAndInjectsResult(t *testing.T) {
	auth := &fakeAuth{token: "subscription-token", account: "account-1"}
	searchRequests := 0
	service := serviceFor(t, auth, func(request *http.Request) (*http.Response, error) {
		searchRequests++
		if request.URL.String() != "https://chatgpt.com/backend-api/codex/responses" {
			t.Errorf("search URL = %s", request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer subscription-token" || request.Header.Get("Chatgpt-Account-Id") != "account-1" {
			t.Errorf("search auth headers were not supplied")
		}
		if request.Header.Get("X-Codex-Turn-Metadata") != "turn-metadata" || request.Header.Get("User-Agent") != "codex-test" {
			t.Errorf("selected caller headers were not forwarded: %v", request.Header)
		}
		if request.Header.Get("X-Unrelated") != "" || request.Header.Get("Authorization") == "Bearer caller-secret" {
			t.Errorf("unsafe caller headers were forwarded: %v", request.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != DefaultModel || body["stream"] != true || body["store"] != false {
			t.Errorf("unexpected sidecar body: %#v", body)
		}
		reasoning := body["reasoning"].(map[string]any)
		if reasoning["effort"] != DefaultReasoning {
			t.Errorf("reasoning = %#v", reasoning)
		}
		tool := body["tools"].([]any)[0].(map[string]any)
		if tool["type"] != "web_search" || tool["search_context_size"] != "high" {
			t.Errorf("hosted tool was not replayed: %#v", tool)
		}
		stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Today changed.\\nSources:\\n- Example: https://example.com/news\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"
		return httpResponse(http.StatusOK, "text/event-stream", stream), nil
	}, nil)

	rounds := 0
	backend := BackendFunc(func(_ context.Context, body []byte) (*http.Response, error) {
		rounds++
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		tools := request["tools"].([]any)
		if rounds == 1 {
			last := tools[len(tools)-1].(map[string]any)
			if last["type"] != "function" || last["name"] != "web_search" {
				t.Fatalf("routed model did not receive synthetic tool: %#v", last)
			}
			return searchCallResponse("call_search", "today's changes"), nil
		}
		encoded := string(body)
		if !strings.Contains(encoded, `"type":"function_call_output"`) || !strings.Contains(encoded, "UNTRUSTED web content") || !strings.Contains(encoded, "https://example.com/news") {
			t.Errorf("search result not injected: %s", encoded)
		}
		return finalResponse("Final answer with https://example.com/news"), nil
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(requestBody(false)))
	request.Header.Set("Authorization", "Bearer caller-secret")
	request.Header.Set("X-Codex-Turn-Metadata", "turn-metadata")
	request.Header.Set("User-Agent", "codex-test")
	request.Header.Set("X-Unrelated", "do-not-forward")
	service.ServeResponses(recorder, request, requestBody(false), backend)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Final answer") {
		t.Fatalf("response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if rounds != 2 || searchRequests != 1 {
		t.Fatalf("rounds=%d search requests=%d", rounds, searchRequests)
	}
}

func TestNoHostedSearchIsBytePreserving(t *testing.T) {
	auth := &fakeAuth{token: "token", account: "account"}
	service := serviceFor(t, auth, func(*http.Request) (*http.Response, error) {
		t.Fatal("search sidecar must not run")
		return nil, nil
	}, nil)
	body := []byte(`{"model":"modal/model","input":"hello","unknown":123}`)
	backend := BackendFunc(func(_ context.Context, received []byte) (*http.Response, error) {
		if !bytes.Equal(received, body) {
			t.Fatalf("body changed: %s", received)
		}
		response := httpResponse(http.StatusAccepted, "application/octet-stream", "opaque-response")
		response.Header.Set("X-Upstream", "kept")
		return response, nil
	})
	recorder := httptest.NewRecorder()
	service.ServeResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)), body, backend)
	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "opaque-response" || recorder.Header().Get("X-Upstream") != "kept" {
		t.Fatalf("response status=%d headers=%v body=%q", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestSearchLimitForcesFinalAnswer(t *testing.T) {
	auth := &fakeAuth{token: "token", account: "account"}
	searches := 0
	service := serviceFor(t, auth, func(*http.Request) (*http.Response, error) {
		searches++
		return httpResponse(http.StatusOK, "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"result\"}\n\n"), nil
	}, func(config *Config) { config.MaxSearches = 2 })
	rounds := 0
	backend := BackendFunc(func(_ context.Context, body []byte) (*http.Response, error) {
		rounds++
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("invalid routed request: %v: %s", err, body)
		}
		tools, _ := request["tools"].([]any)
		hasSearch := false
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			if tool["name"] == "web_search" {
				hasSearch = true
			}
		}
		if rounds <= 2 {
			if !hasSearch {
				t.Fatal("search tool removed before limit")
			}
			return searchCallResponse("call_"+strconv.Itoa(rounds), "query"+strconv.Itoa(rounds)), nil
		}
		if hasSearch {
			t.Fatal("forced-answer round still had web_search")
		}
		return finalResponse("forced answer"), nil
	})
	recorder := httptest.NewRecorder()
	service.ServeResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), requestBody(false), backend)
	if recorder.Code != http.StatusOK || rounds != 3 || searches != 2 || !strings.Contains(recorder.Body.String(), "forced answer") {
		t.Fatalf("status=%d rounds=%d searches=%d body=%s", recorder.Code, rounds, searches, recorder.Body.String())
	}
}

func TestMixedRealToolCallIsPreservedAndSearchCallHidden(t *testing.T) {
	auth := &fakeAuth{token: "token", account: "account"}
	service := serviceFor(t, auth, func(*http.Request) (*http.Response, error) {
		t.Fatal("mixed calls must not start a hidden search loop")
		return nil, nil
	}, nil)
	real := map[string]any{"type": "function_call", "name": "exec", "call_id": "call_exec", "arguments": `{"cmd":"date"}`}
	backend := BackendFunc(func(context.Context, []byte) (*http.Response, error) {
		return searchCallResponse("call_search", "today", real), nil
	})
	recorder := httptest.NewRecorder()
	service.ServeResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), requestBody(true), backend)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"name":"exec"`) || strings.Contains(recorder.Body.String(), `"name":"web_search"`) {
		t.Fatalf("mixed response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("rebuilt stream is incomplete: %s", recorder.Body.String())
	}
}

func TestSearchRefreshesRejectedSubscriptionToken(t *testing.T) {
	auth := &fakeAuth{token: "old", newToken: "new", account: "account"}
	sends := 0
	service := serviceFor(t, auth, func(request *http.Request) (*http.Response, error) {
		sends++
		if sends == 1 {
			if request.Header.Get("Authorization") != "Bearer old" {
				t.Fatal("first token missing")
			}
			return httpResponse(http.StatusUnauthorized, "application/json", `{"error":{"type":"unauthorized"}}`), nil
		}
		if request.Header.Get("Authorization") != "Bearer new" {
			t.Fatal("refreshed token missing")
		}
		return httpResponse(http.StatusOK, "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"), nil
	}, nil)
	outcome := service.search(context.Background(), "query", map[string]any{"type": "web_search"})
	if outcome.err != "" || outcome.text != "ok" || sends != 2 || auth.refresh != 1 {
		t.Fatalf("outcome=%#v sends=%d refresh=%d", outcome, sends, auth.refresh)
	}
}

func TestSearchRetries429WithinBound(t *testing.T) {
	auth := &fakeAuth{token: "token", account: "account"}
	sends := 0
	service := serviceFor(t, auth, func(*http.Request) (*http.Response, error) {
		sends++
		if sends < 3 {
			response := httpResponse(http.StatusTooManyRequests, "application/json", `{}`)
			response.Header.Set("Retry-After", "0")
			return response, nil
		}
		return httpResponse(http.StatusOK, "text/event-stream", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"), nil
	}, nil)
	outcome := service.search(context.Background(), "query", map[string]any{"type": "web_search"})
	if outcome.err != "" || sends != maxSends {
		t.Fatalf("outcome=%#v sends=%d", outcome, sends)
	}
}

func TestSearchRejectsNonChatGPTCredentialDestination(t *testing.T) {
	_, err := New(Config{Auth: &fakeAuth{}, UpstreamBase: "https://example.com/backend-api/codex"})
	if err == nil || !strings.Contains(err.Error(), "chatgpt.com") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCancelledSearchStopsTransport(t *testing.T) {
	auth := &fakeAuth{token: "token", account: "account"}
	service := serviceFor(t, auth, func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}, func(config *Config) { config.Timeout = time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := service.search(ctx, "query", map[string]any{"type": "web_search"})
	if !strings.Contains(outcome.err, "timed out or was cancelled") {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestFormattingBoundsAndRejectsUnsafeCitations(t *testing.T) {
	long := strings.Repeat("x", maxAnswerChars+100)
	formatted := formatResults([]queryResult{{query: "<query>", outcome: searchOutcome{text: long, sources: []source{
		{Title: "safe", URL: "https://example.com"}, {Title: "unsafe", URL: "javascript:alert(1)"},
	}}}})
	if !strings.Contains(formatted, "[truncated]") || strings.Contains(formatted, "<query>") || strings.Contains(formatted, "javascript:") || !strings.Contains(formatted, "https://example.com") {
		t.Fatalf("formatted result was not safely bounded: %s", formatted)
	}
}
