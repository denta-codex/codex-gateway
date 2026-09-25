package modal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/denta-codex/codex-gateway/adapter"
)

// responseCapture turns one translated Modal iteration back into an
// *http.Response for the web-search orchestrator. It is used only on search
// turns; ordinary Modal turns retain their direct streaming path.
type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
	err    error
}

func newResponseCapture() *responseCapture {
	return &responseCapture{header: make(http.Header)}
}

func (c *responseCapture) Header() http.Header { return c.header }

func (c *responseCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *responseCapture) Write(data []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.body.Len()+len(data) > maxTranslatedPayload {
		c.err = errors.New("translated Modal response exceeds adapter limit")
		return 0, c.err
	}
	return c.body.Write(data)
}

func (c *responseCapture) Flush() {}

func (c *responseCapture) response() (*http.Response, error) {
	if c.err != nil {
		return nil, c.err
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     c.header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(c.body.Bytes())),
	}, nil
}

// eventHTTPSink is intentionally confined to the Modal adapter. The hosted
// search helper still orchestrates HTTP Responses rounds, while the public
// adapter boundary remains transport-neutral.
type eventHTTPSink struct{ capture *responseCapture }

func (s eventHTTPSink) Emit(event json.RawMessage) error {
	s.capture.Header().Set("Content-Type", "text/event-stream")
	_, err := s.capture.Write(append(append([]byte("data: "), event...), []byte("\n\n")...))
	return err
}

func httpRequestFrom(ctx context.Context, request adapter.Request) *http.Request {
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", nil)
	for key, values := range request.Metadata {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	return r
}

func writeCapturedError(capture *responseCapture, err error) {
	status, code, message := http.StatusBadGateway, "adapter_error", "adapter request failed"
	var public *adapter.Error
	if errors.As(err, &public) {
		status, code, message = public.Status, public.Code, public.Message
	}
	capture.Header().Set("Content-Type", "application/json")
	capture.WriteHeader(status)
	data, _ := json.Marshal(map[string]any{"error": map[string]any{"type": code, "code": code, "message": message}})
	_, _ = capture.Write(data)
}

func capturedResponseEvents(response *http.Response, sink adapter.EventSink) error {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTranslatedPayload+1))
	if err != nil || len(body) > maxTranslatedPayload {
		return adapter.NewError(http.StatusBadGateway, "web_search_invalid_response", "web search returned an invalid response", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			Error struct {
				Code    string `json:"code"`
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &payload)
		code := payload.Error.Code
		if code == "" {
			code = payload.Error.Type
		}
		if code == "" {
			code = "web_search_failed"
		}
		message := payload.Error.Message
		if message == "" {
			message = "web search failed"
		}
		return adapter.NewError(response.StatusCode, code, message, nil)
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") || bytes.HasPrefix(bytes.TrimSpace(body), []byte("data:")) {
		scanner := bufio.NewScanner(bytes.NewReader(body))
		scanner.Buffer(make([]byte, 64<<10), maxTranslatedPayload)
		var lines []string
		emit := func() error {
			if len(lines) == 0 {
				return nil
			}
			data := strings.Join(lines, "\n")
			lines = lines[:0]
			if data == "[DONE]" {
				return nil
			}
			if !json.Valid([]byte(data)) {
				return errors.New("web search emitted malformed Responses event")
			}
			return sink.Emit(json.RawMessage(data))
		}
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if err := emit(); err != nil {
					return err
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		return emit()
	}
	if !json.Valid(body) {
		return adapter.NewError(http.StatusBadGateway, "web_search_invalid_response", "web search returned invalid JSON", nil)
	}
	event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": json.RawMessage(body)})
	return sink.Emit(event)
}
