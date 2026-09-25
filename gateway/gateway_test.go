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
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/denta-codex/codex-gateway/adapters/example"
	"github.com/denta-codex/codex-gateway/internal/catalog"
	"github.com/denta-codex/codex-gateway/internal/subscription"
	"github.com/klauspost/compress/zstd"
)

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

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

func TestCatalogETagAndConditionalRequest(t *testing.T) {
	path := testCatalog(t)
	g, err := New(Config{CatalogPath: path, Auth: testAuth(t), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g)
	defer server.Close()

	response, err := http.Get(server.URL + "/v1/models?client_version=test")
	if err != nil {
		t.Fatal(err)
	}
	firstETag := response.Header.Get("ETag")
	response.Body.Close()
	if response.StatusCode != http.StatusOK || firstETag == "" || response.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("status=%d etag=%q cache=%q", response.StatusCode, firstETag, response.Header.Get("Cache-Control"))
	}

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	request.Header.Set("If-None-Match", firstETag)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional status=%d", response.StatusCode)
	}

	if err := os.WriteFile(path, []byte(`{"models":[{"slug":"gpt-6-sol"},{"slug":"new/model"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") == firstETag {
		t.Fatalf("updated status=%d etag=%q", response.StatusCode, response.Header.Get("ETag"))
	}
}

func TestResponsesCarryInstalledCatalogETag(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Models-Etag", `"upstream"`)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer upstream.Close()
	path := testCatalog(t)
	snapshot, err := catalog.ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(Config{UpstreamBase: upstream.URL, CatalogPath: path, Auth: testAuth(t), Client: upstream.Client(), Logger: log.New(io.Discard, "", 0)}, example.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g)
	defer server.Close()

	for _, model := range []string{"gpt-6-sol", "example/echo"} {
		response, err := http.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"`+model+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK || response.Header.Get("X-Models-Etag") != snapshot.ETag {
			t.Fatalf("model=%s status=%d etag=%q", model, response.StatusCode, response.Header.Get("X-Models-Etag"))
		}
	}
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
	var logs lockedBuffer
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

func TestZstdModelRoutingAndNativeBytePreservation(t *testing.T) {
	var nativeBody []byte
	var nativeEncoding string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeBody, _ = io.ReadAll(r.Body)
		nativeEncoding = r.Header.Get("Content-Encoding")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamBase: upstream.URL + "/backend-api/codex", CatalogPath: testCatalog(t), Auth: testAuth(t), Client: upstream.Client(), Logger: log.New(io.Discard, "", 0)}, example.Adapter{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g)
	defer server.Close()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	for _, model := range []string{"example/echo", "gpt-6-sol"} {
		plain := []byte(`{"model":"` + model + `","input":"hello"}`)
		compressed := encoder.EncodeAll(plain, nil)
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(compressed))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Content-Encoding", "zstd")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("%s status=%d", model, response.StatusCode)
		}
		if model == "example/echo" && nativeBody != nil {
			t.Fatal("namespaced compressed request reached native upstream")
		}
		if model == "gpt-6-sol" && (!bytes.Equal(nativeBody, compressed) || nativeEncoding != "zstd") {
			t.Fatal("native compressed request changed in transit")
		}
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader("invalid zstd"))
	request.Header.Set("Content-Encoding", "zstd")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatalf("malformed zstd status=%d", response.StatusCode)
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

func TestWebSocketMixesStatelessAdapterAndNativeRequests(t *testing.T) {
	var upstreamConnections atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamConnections.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for {
			messageType, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			if err := conn.Write(context.Background(), messageType, data); err != nil {
				t.Error(err)
				return
			}
		}
	}))
	defer upstream.Close()
	g, err := New(Config{UpstreamBase: upstream.URL, CatalogPath: testCatalog(t), Auth: testAuth(t), Client: upstream.Client(), Logger: log.New(io.Discard, "", 0)}, example.Adapter{})
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

	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"example/echo","stream_id":"adapter-1","input":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var adapterEvent struct {
		Type     string `json:"type"`
		StreamID string `json:"stream_id"`
	}
	if json.Unmarshal(data, &adapterEvent) != nil || adapterEvent.Type != "response.completed" || adapterEvent.StreamID != "adapter-1" {
		t.Fatalf("adapter event=%s", data)
	}
	if upstreamConnections.Load() != 0 {
		t.Fatal("adapter request eagerly opened the native WebSocket")
	}

	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"example/echo","stream_id":"adapter-2","previous_response_id":"example-response","input":"again"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var missing struct {
		Type     string `json:"type"`
		StreamID string `json:"stream_id"`
		Error    struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &missing) != nil || missing.Type != "error" || missing.StreamID != "adapter-2" || missing.Error.Code != "previous_response_not_found" {
		t.Fatalf("continuation error=%s", data)
	}

	native := []byte(`{"type":"response.create","model":"gpt-6-sol","stream_id":"native-1","future_field":true}`)
	if err := conn.Write(context.Background(), websocket.MessageText, native); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, native) || upstreamConnections.Load() != 1 {
		t.Fatalf("native message=%s connections=%d", data, upstreamConnections.Load())
	}
}

func TestNativeWebSocketRewritesModelsETagMetadata(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(context.Background()); err != nil {
			t.Error(err)
			return
		}
		metadata := []byte(`{"type":"codex.response.metadata","headers":{"X-Models-Etag":"upstream","x-other":"keep"},"sequence_number":1}`)
		if err := conn.Write(context.Background(), websocket.MessageText, metadata); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	path := testCatalog(t)
	snapshot, err := catalog.ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(Config{UpstreamBase: upstream.URL, CatalogPath: path, Auth: testAuth(t), Client: upstream.Client(), Logger: log.New(io.Discard, "", 0)})
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
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-6-sol"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Headers["x-models-etag"] != snapshot.ETag || metadata.Headers["x-other"] != "keep" {
		t.Fatalf("metadata=%s", data)
	}
}
