// Package gateway serves the Codex-facing Responses endpoint.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/denta-codex/codex-gateway/adapter"
	"github.com/denta-codex/codex-gateway/internal/subscription"
)

const MaxRequestBytes int64 = 128 << 20

type Config struct {
	UpstreamBase string
	CatalogPath  string
	Auth         *subscription.Auth
	Logger       *log.Logger
	Client       *http.Client
}

type Gateway struct {
	config    Config
	upstream  *url.URL
	adapters  map[string]adapter.Adapter
	requests  atomic.Uint64
	fallbacks atomic.Uint64
	started   time.Time
}

func New(config Config, adapters ...adapter.Adapter) (*Gateway, error) {
	if config.UpstreamBase == "" {
		config.UpstreamBase = "https://chatgpt.com/backend-api/codex"
	}
	upstream, err := url.Parse(config.UpstreamBase)
	if err != nil || upstream.Scheme != "https" || upstream.Host == "" || upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, errors.New("subscription upstream must be a fixed HTTPS origin and path")
	}
	if config.Auth == nil || config.CatalogPath == "" {
		return nil, errors.New("auth and catalog path are required")
	}
	if config.Logger == nil {
		config.Logger = log.Default()
	}
	if config.Client == nil {
		config.Client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			ResponseHeaderTimeout: 90 * time.Second,
		}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	registry := map[string]adapter.Adapter{}
	for _, extension := range adapters {
		name := extension.Namespace()
		if name == "" || strings.ContainsAny(name, "/ \\:") || registry[name] != nil {
			return nil, fmt.Errorf("invalid or duplicate adapter namespace %q", name)
		}
		registry[name] = extension
	}
	return &Gateway{config: config, upstream: upstream, adapters: registry, started: time.Now()}, nil
}

func requestID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func (g *Gateway) event(name string, fields map[string]any) {
	fields["event"] = name
	data, _ := json.Marshal(fields)
	g.config.Logger.Print(string(data))
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": code, "message": message}})
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "requests": g.requests.Load(), "fallbacks": g.fallbacks.Load(), "started_at": g.started.UTC().Format(time.RFC3339)})
		return
	}
	if r.URL.Path == "/ready" && r.Method == http.MethodGet {
		if _, err := os.Stat(g.config.CatalogPath); err != nil {
			writeError(w, 503, "catalog_unavailable", "model catalog unavailable")
			return
		}
		if _, _, err := g.config.Auth.Token(r.Context()); err != nil {
			writeError(w, 503, "subscription_auth_unavailable", "Grace needs codex login")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"ready\":true}\n"))
		return
	}
	if (r.URL.Path == "/v1/models" || r.URL.Path == "/model-catalog.json") && r.Method == http.MethodGet {
		data, err := os.ReadFile(g.config.CatalogPath)
		if err != nil {
			writeError(w, 503, "catalog_unavailable", "model catalog unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		writeError(w, 404, "unknown_route", "unknown gateway route")
		return
	}
	for _, segment := range strings.Split(r.URL.Path, "/") {
		if segment == "." || segment == ".." {
			writeError(w, 400, "invalid_path", "invalid gateway path")
			return
		}
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/responses" && websocketUpgrade(r) {
		g.serveWebSocket(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, 405, "unsupported_method", "unsupported method")
		return
	}
	g.serveHTTP(w, r)
}

func modelFromBody(body []byte) (string, error) {
	var value struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return "", err
	}
	return value.Model, nil
}

func (g *Gateway) adapterFor(model string) (adapter.Adapter, bool) {
	namespace, _, namespaced := strings.Cut(model, "/")
	if !namespaced {
		return nil, false
	}
	return g.adapters[namespace], true
}

var hopHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
}

func copyRequestHeaders(dst, src http.Header) {
	for _, key := range []string{"Accept", "Content-Type", "OpenAI-Beta", "X-OpenAI-Internal-Codex-Responses-Lite", "X-OpenAI-Memgen-Request", "User-Agent", "Session-Id", "Originator"} {
		for _, value := range src.Values(key) {
			dst.Add(key, value)
		}
	}
	dst.Set("Accept-Encoding", "identity")
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if hopHeaders[lower] || lower == "set-cookie" {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (g *Gateway) target(path, query string) string {
	copy := *g.upstream
	copy.Path = strings.TrimRight(g.upstream.Path, "/") + strings.TrimPrefix(path, "/v1")
	copy.RawQuery = query
	return copy.String()
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	id, started := requestID(), time.Now()
	g.requests.Add(1)
	limited := http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	defer limited.Close()
	body, err := io.ReadAll(limited)
	if err != nil {
		writeError(w, 413, "request_too_large", "request body exceeds gateway limit")
		g.event("request_rejected", map[string]any{"request_id": id, "path": r.URL.Path, "reason": "request_size"})
		return
	}
	model := ""
	if r.Method == http.MethodPost && (r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/responses/compact") {
		model, err = modelFromBody(body)
		if err != nil {
			writeError(w, 400, "invalid_json", "Responses body must be JSON")
			return
		}
		if extension, namespaced := g.adapterFor(model); namespaced {
			if extension == nil {
				writeError(w, 404, "adapter_unavailable", "no adapter registered for model namespace")
				return
			}
			if r.URL.Path == "/v1/responses/compact" {
				writeError(w, 501, "adapter_compaction_unsupported", "adapter does not support Responses compaction")
				return
			}
			extension.ServeResponses(w, r, body)
			g.event("adapter_request", map[string]any{"request_id": id, "path": r.URL.Path, "adapter": extension.Namespace(), "model": model, "duration_ms": time.Since(started).Milliseconds()})
			return
		}
	}
	if r.URL.Path != "/v1/responses" && r.URL.Path != "/v1/responses/compact" {
		g.fallbacks.Add(1)
		g.event("fallback_route", map[string]any{"request_id": id, "method": r.Method, "path": r.URL.Path})
	}
	token, account, err := g.config.Auth.Token(r.Context())
	if err != nil {
		writeError(w, 503, "subscription_auth_unavailable", "Grace needs codex login")
		return
	}
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), r.Method, g.target(r.URL.Path, r.URL.RawQuery), bytes.NewReader(body))
	if err != nil {
		writeError(w, 502, "upstream_request_failed", "could not build upstream request")
		return
	}
	copyRequestHeaders(upstreamRequest.Header, r.Header)
	upstreamRequest.Header.Set("Authorization", "Bearer "+token)
	upstreamRequest.Header.Set("Chatgpt-Account-Id", account)
	upstreamStarted := time.Now()
	response, err := g.config.Client.Do(upstreamRequest)
	if err != nil {
		writeError(w, 502, "upstream_unavailable", "subscription upstream unavailable")
		g.event("upstream_error", map[string]any{"request_id": id, "path": r.URL.Path, "error_type": fmt.Sprintf("%T", err)})
		return
	}
	defer response.Body.Close()
	upstreamHeadersMS := time.Since(upstreamStarted).Milliseconds()
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	observer := newUsageObserver()
	writer := &streamWriter{Writer: w, flusher: flushOf(w), observer: observer}
	_, copyErr := io.Copy(writer, response.Body)
	observer.Finish()
	fields := map[string]any{"request_id": id, "method": r.Method, "path": r.URL.Path, "model": model, "provider": "subscription", "transport": "http", "status": response.StatusCode, "request_bytes": len(body), "upstream_response_headers_ms": upstreamHeadersMS, "duration_ms": time.Since(started).Milliseconds()}
	if observer.complete {
		fields["response_event"] = observer.event
		fields["actual_service_tier"] = observer.serviceTier
		fields["input_tokens"] = observer.input
		fields["output_tokens"] = observer.output
		fields["total_tokens"] = observer.total
	}
	if copyErr != nil {
		fields["stream_error_type"] = fmt.Sprintf("%T", copyErr)
	}
	g.event("request_complete", fields)
}

func flushOf(w http.ResponseWriter) http.Flusher { f, _ := w.(http.Flusher); return f }

type streamWriter struct {
	io.Writer
	flusher  http.Flusher
	observer *usageObserver
}

func (w *streamWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	if n > 0 {
		w.observer.Feed(data[:n])
		if w.flusher != nil {
			w.flusher.Flush()
		}
	}
	return n, err
}

type usageObserver struct {
	buffer               []byte
	input, output, total int64
	complete             bool
	disabled             bool
	event, serviceTier   string
}

func newUsageObserver() *usageObserver { return &usageObserver{} }
func (o *usageObserver) Feed(data []byte) {
	if o.disabled {
		return
	}
	if len(o.buffer)+len(data) > 1<<20 {
		o.disabled = true
		o.buffer = nil
		return
	}
	o.buffer = append(o.buffer, data...)
	for {
		end := bytes.IndexByte(o.buffer, '\n')
		if end < 0 {
			return
		}
		line := bytes.TrimSpace(o.buffer[:end])
		o.buffer = o.buffer[end+1:]
		o.parseLine(line)
	}
}

func (o *usageObserver) Finish() {
	if !o.disabled && len(o.buffer) > 0 {
		o.parseLine(bytes.TrimSpace(o.buffer))
	}
}

func (o *usageObserver) parseLine(line []byte) {
	line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	var event struct {
		Type     string `json:"type"`
		Response struct {
			ServiceTier string `json:"service_tier"`
			Usage       struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
				Total  int64 `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(line, &event) != nil {
		return
	}
	if event.Type != "response.completed" && event.Type != "response.failed" && event.Type != "response.incomplete" {
		return
	}
	o.input, o.output, o.total = event.Response.Usage.Input, event.Response.Usage.Output, event.Response.Usage.Total
	o.serviceTier, o.event, o.complete = event.Response.ServiceTier, event.Type, true
}

func websocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (g *Gateway) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	id, started := requestID(), time.Now()
	g.requests.Add(1)
	token, account, err := g.config.Auth.Token(r.Context())
	if err != nil {
		writeError(w, 503, "subscription_auth_unavailable", "Grace needs codex login")
		return
	}
	target, err := url.Parse(g.target(r.URL.Path, r.URL.RawQuery))
	if err != nil {
		writeError(w, 502, "upstream_request_failed", "invalid subscription URL")
		return
	}
	target.Scheme = "wss"
	headers := http.Header{}
	copyRequestHeaders(headers, r.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Chatgpt-Account-Id", account)
	upstream, response, err := websocket.Dial(r.Context(), target.String(), &websocket.DialOptions{HTTPHeader: headers, HTTPClient: g.config.Client})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		writeError(w, 502, "upstream_unavailable", "subscription WebSocket unavailable")
		g.event("websocket_error", map[string]any{"request_id": id, "upstream_status": status})
		return
	}
	defer upstream.CloseNow()
	upstream.SetReadLimit(MaxRequestBytes)
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(MaxRequestBytes)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- pump(ctx, client, upstream, true, nil) }()
	go func() {
		results <- pump(ctx, upstream, client, false, func(data []byte) {
			var message struct {
				Type     string `json:"type"`
				StreamID string `json:"stream_id"`
			}
			if json.Unmarshal(data, &message) != nil {
				return
			}
			observer := newUsageObserver()
			observer.parseLine(data)
			if !observer.complete {
				return
			}
			g.event("request_complete", map[string]any{"request_id": id, "path": r.URL.Path, "provider": "subscription", "transport": "websocket", "status": 200, "stream_id": message.StreamID, "response_event": observer.event, "actual_service_tier": observer.serviceTier, "input_tokens": observer.input, "output_tokens": observer.output, "total_tokens": observer.total, "duration_ms": time.Since(started).Milliseconds()})
		})
	}()
	err = <-results
	cancel()
	_ = client.Close(websocket.StatusNormalClosure, "")
	_ = upstream.Close(websocket.StatusNormalClosure, "")
	fields := map[string]any{"request_id": id, "path": r.URL.Path, "duration_ms": time.Since(started).Milliseconds()}
	if err != nil && !errors.Is(err, context.Canceled) {
		fields["close_error_type"] = fmt.Sprintf("%T", err)
	}
	g.event("websocket_closed", fields)
}

func pump(ctx context.Context, from, to *websocket.Conn, inspect bool, onEvent func([]byte)) error {
	for {
		typeID, data, err := from.Read(ctx)
		if err != nil {
			return err
		}
		if inspect && typeID == websocket.MessageText {
			var message struct {
				Type  string `json:"type"`
				Model string `json:"model"`
			}
			if json.Unmarshal(data, &message) == nil && message.Type == "response.create" && strings.Contains(message.Model, "/") {
				// Adapter WebSocket transport must be implemented by that adapter; never
				// leak a namespaced model request to the subscription upstream.
				return errors.New("adapter WebSocket transport unavailable")
			}
		}
		if err := to.Write(ctx, typeID, data); err != nil {
			return err
		}
		if onEvent != nil && typeID == websocket.MessageText {
			onEvent(data)
		}
	}
}
