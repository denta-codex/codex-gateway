package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/denta-codex/codex-gateway/adapters/example"
	"github.com/denta-codex/codex-gateway/internal/subscription"
)

func testAuth(t *testing.T) *subscription.Auth {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"grace-token","account_id":"grace-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return &subscription.Auth{Path: path}
}

func testCatalog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{"models":[{"slug":"gpt-6-sol"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSubscriptionPassthroughAndFallback(t *testing.T) {
	var mu sync.Mutex
	var got []struct{ method, path, query, body, auth, account, cookie string }
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, struct{ method, path, query, body, auth, account, cookie string }{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Get("Authorization"), r.Header.Get("Chatgpt-Account-Id"), r.Header.Get("Cookie")})
		mu.Unlock()
		if r.URL.Path == "/backend-api/codex/responses" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n")
			return
		}
		_, _ = io.WriteString(w, "opaque fallback")
	}))
	defer upstream.Close()
	var logs bytes.Buffer
	g, err := New(Config{UpstreamBase: upstream.URL + "/backend-api/codex", CatalogPath: testCatalog(t), Auth: testAuth(t), Client: upstream.Client(), Logger: log.New(&logs, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g)
	defer server.Close()
	body := `{ "model" : "gpt-6-sol", "future_field": {"keep":true}, "stream":true }`
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer caller-secret")
	request.Header.Set("Chatgpt-Account-Id", "caller-account")
	request.Header.Set("Cookie", "caller-private")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(result, []byte("response.completed")) {
		t.Fatalf("response=%d %q", response.StatusCode, result)
	}
	response, err = http.Get(server.URL + "/v1/new-feature?opaque=a%2Bb")
	if err != nil {
		t.Fatal(err)
	}
	fallback, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(fallback) != "opaque fallback" {
		t.Fatalf("fallback=%q", fallback)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("upstream calls=%d", len(got))
	}
	if got[0].body != body || got[0].path != "/backend-api/codex/responses" || got[0].auth != "Bearer grace-token" || got[0].account != "grace-account" || got[0].cookie != "" {
		t.Fatalf("subscription request not preserved or isolated: %+v", got[0])
	}
	if got[1].method != "GET" || got[1].path != "/backend-api/codex/new-feature" || got[1].query != "opaque=a%2Bb" {
		t.Fatalf("fallback target=%+v", got[1])
	}
	if !strings.Contains(logs.String(), `"event":"fallback_route"`) || !strings.Contains(logs.String(), `"total_tokens":5`) || strings.Contains(logs.String(), "caller-secret") {
		t.Fatalf("telemetry=%s", logs.String())
	}
}

func TestAdapterDispatchAndMissingNamespace(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls++ }))
	defer upstream.Close()
	g, err := New(Config{UpstreamBase: upstream.URL + "/backend-api/codex", CatalogPath: testCatalog(t), Auth: testAuth(t), Client: upstream.Client(), Logger: log.New(io.Discard, "", 0)}, example.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g)
	defer server.Close()
	for _, test := range []struct {
		model  string
		status int
	}{{"example/echo", 200}, {"modal/missing", 404}} {
		data, _ := json.Marshal(map[string]string{"model": test.model})
		response, err := http.Post(server.URL+"/v1/responses", "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("%s status=%d", test.model, response.StatusCode)
		}
	}
	compact, err := http.Post(server.URL+"/v1/responses/compact", "application/json", strings.NewReader(`{"model":"example/echo"}`))
	if err != nil {
		t.Fatal(err)
	}
	compact.Body.Close()
	if compact.StatusCode != 501 {
		t.Fatalf("namespaced compact status=%d", compact.StatusCode)
	}
	if upstreamCalls != 0 {
		t.Fatalf("adapter request leaked to subscription: %d", upstreamCalls)
	}
}

func TestNativeWebSocketPassthrough(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" || r.Header.Get("Authorization") != "Bearer grace-token" {
			t.Errorf("upstream WebSocket request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		typeID, data, err := conn.Read(context.Background())
		if err == nil {
			err = conn.Write(context.Background(), typeID, data)
		}
		if err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamBase: upstream.URL + "/backend-api/codex", CatalogPath: testCatalog(t), Auth: testAuth(t), Client: upstream.Client(), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g)
	defer server.Close()
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	message := []byte(`{"type":"response.create","model":"gpt-6-sol","future_field":true}`)
	if err := conn.Write(context.Background(), websocket.MessageText, message); err != nil {
		t.Fatal(err)
	}
	_, echo, err := conn.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(echo, message) {
		t.Fatalf("WebSocket changed message: %q", echo)
	}
}
