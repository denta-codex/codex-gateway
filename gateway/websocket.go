package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/denta-codex/codex-gateway/adapter"
)

type websocketSession struct {
	g       *Gateway
	request *http.Request
	client  *websocket.Conn
	id      string
	started time.Time
	ctx     context.Context
	cancel  context.CancelFunc

	writeMu sync.Mutex
	native  *websocket.Conn

	adaptersMu sync.Mutex
	active     map[string]*activeAdapter
}

type activeAdapter struct{ cancel context.CancelFunc }

func newWebSocketSession(g *Gateway, request *http.Request, client *websocket.Conn, id string, started time.Time, ctx context.Context, cancel context.CancelFunc) *websocketSession {
	return &websocketSession{g: g, request: request, client: client, id: id, started: started, ctx: ctx, cancel: cancel, active: map[string]*activeAdapter{}}
}

type websocketRequest struct {
	Type     string `json:"type"`
	Model    string `json:"model"`
	StreamID string `json:"stream_id"`
}

func (s *websocketSession) run() error {
	for {
		messageType, data, err := s.client.Read(s.ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageText {
			if err := s.writeNative(messageType, data); err != nil {
				return err
			}
			continue
		}
		var request websocketRequest
		if json.Unmarshal(data, &request) == nil {
			if request.Type == "response.cancel" && s.cancelAdapter(request.StreamID) {
				continue
			}
			if request.Type == "response.create" {
				extension, namespaced := s.g.adapterFor(request.Model)
				if namespaced {
					if extension == nil {
						_ = s.writeAdapterError(adapter.NewError(http.StatusNotFound, "adapter_unavailable", "No adapter is registered for this model namespace.", nil), request.StreamID)
						continue
					}
					if previousResponseID(data) != "" {
						_ = s.writeAdapterError(adapter.NewError(http.StatusNotFound, "previous_response_not_found", "Previous response not found. Retry with self-contained input.", nil), request.StreamID)
						continue
					}
					s.startAdapter(extension, data, request.StreamID)
					continue
				}
			}
		}
		if err := s.writeNative(messageType, data); err != nil {
			streamID := request.StreamID
			if writeErr := s.writeAdapterError(adapter.NewError(http.StatusBadGateway, "upstream_unavailable", "Subscription WebSocket unavailable.", err), streamID); writeErr != nil {
				return writeErr
			}
		}
	}
}

func (s *websocketSession) startAdapter(extension adapter.Adapter, body []byte, streamID string) {
	ctx, cancel := context.WithCancel(s.ctx)
	current := &activeAdapter{cancel: cancel}
	s.adaptersMu.Lock()
	if s.active[streamID] != nil {
		s.adaptersMu.Unlock()
		cancel()
		_ = s.writeAdapterError(adapter.NewError(http.StatusConflict, "adapter_stream_busy", "An adapter response is already active on this stream.", nil), streamID)
		return
	}
	s.active[streamID] = current
	s.adaptersMu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.adaptersMu.Lock()
			if s.active[streamID] == current {
				delete(s.active, streamID)
			}
			s.adaptersMu.Unlock()
		}()
		started := time.Now()
		err := extension.ServeResponses(ctx, adapterRequest(body, s.request.Header), websocketEventSink{session: s, streamID: streamID})
		fields := map[string]any{"request_id": s.id, "path": s.request.URL.Path, "adapter": extension.Namespace(), "transport": "websocket", "stream_id": streamID, "duration_ms": time.Since(started).Milliseconds()}
		if err != nil && !errors.Is(err, context.Canceled) {
			fields["error_code"] = publicAdapterError(err).Code
			_ = s.writeAdapterError(err, streamID)
		}
		s.g.event("adapter_request", fields)
	}()
}

func (s *websocketSession) cancelAdapter(streamID string) bool {
	s.adaptersMu.Lock()
	active := s.active[streamID]
	s.adaptersMu.Unlock()
	if active == nil {
		return false
	}
	active.cancel()
	return true
}

type websocketEventSink struct {
	session  *websocketSession
	streamID string
}

func (s websocketEventSink) Emit(event json.RawMessage) error {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(event, &envelope) != nil {
		return errors.New("adapter emitted invalid JSON")
	}
	if s.streamID != "" {
		streamID, _ := json.Marshal(s.streamID)
		envelope["stream_id"] = streamID
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return s.session.writeClient(websocket.MessageText, data)
}

func (s *websocketSession) writeAdapterError(err error, streamID string) error {
	return s.writeClient(websocket.MessageText, adapterErrorEvent(err, streamID))
}

func (s *websocketSession) writeClient(messageType websocket.MessageType, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.client.Write(s.ctx, messageType, data)
}

func (s *websocketSession) writeNative(messageType websocket.MessageType, data []byte) error {
	if err := s.ensureNative(); err != nil {
		return err
	}
	return s.native.Write(s.ctx, messageType, data)
}

func (s *websocketSession) ensureNative() error {
	if s.native != nil {
		return nil
	}
	token, account, err := s.g.config.Auth.Token(s.ctx)
	if err != nil {
		return errors.New("subscription authentication unavailable")
	}
	target, err := url.Parse(s.g.target(s.request.URL.Path, s.request.URL.RawQuery))
	if err != nil {
		return errors.New("invalid subscription URL")
	}
	target.Scheme = "wss"
	headers := http.Header{}
	copyRequestHeaders(headers, s.request.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Chatgpt-Account-Id", account)
	upstream, response, err := websocket.Dial(s.ctx, target.String(), &websocket.DialOptions{HTTPHeader: headers, HTTPClient: s.g.config.Client})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		s.g.event("websocket_error", map[string]any{"request_id": s.id, "upstream_status": status})
		return fmt.Errorf("dial subscription WebSocket: %w", err)
	}
	upstream.SetReadLimit(MaxRequestBytes)
	s.native = upstream
	go s.readNative(upstream)
	return nil
}

func (s *websocketSession) readNative(upstream *websocket.Conn) {
	for {
		messageType, data, err := upstream.Read(s.ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.cancel()
			}
			return
		}
		if messageType == websocket.MessageText {
			data = s.g.rewriteModelsETagMetadata(data)
			s.observeNative(data)
		}
		if err := s.writeClient(messageType, data); err != nil {
			s.cancel()
			return
		}
	}
}

func (s *websocketSession) observeNative(data []byte) {
	var message struct {
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
	s.g.event("request_complete", map[string]any{"request_id": s.id, "path": s.request.URL.Path, "provider": "subscription", "transport": "websocket", "status": 200, "stream_id": message.StreamID, "response_event": observer.event, "actual_service_tier": observer.serviceTier, "input_tokens": observer.input, "output_tokens": observer.output, "total_tokens": observer.total, "duration_ms": time.Since(s.started).Milliseconds()})
}

func (s *websocketSession) closeNative() {
	if s.native != nil {
		_ = s.native.Close(websocket.StatusNormalClosure, "")
	}
}
