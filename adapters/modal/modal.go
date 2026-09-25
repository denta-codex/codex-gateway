// Package modal translates the OpenAI Responses protocol used by Codex into
// the OpenAI-compatible Chat Completions protocol exposed by Modal inference.
package modal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/denta-codex/codex-gateway/adapter"
	"github.com/denta-codex/codex-gateway/websearch"
)

const defaultBaseURL = "https://inference.us-west.modal.direct/v1"

type Adapter struct {
	// BaseURL and Client are dependency-injection points for tests. Production
	// uses the fixed Modal endpoint and a client with no whole-request timeout,
	// since inference streams may be long lived.
	BaseURL        string
	CredentialFile string
	Token          string
	Client         *http.Client
	SearchClient   *http.Client
	Auth           adapter.TokenSource
}

func (Adapter) Namespace() string       { return Namespace }
func (Adapter) Models() []adapter.Model { return models() }

func (a *Adapter) SetSubscriptionAuth(auth adapter.TokenSource) { a.Auth = auth }

func (a Adapter) ServeResponses(ctx context.Context, request adapter.Request, sink adapter.EventSink) error {
	searchEnabled, _ := websearch.Enabled(request.Body)
	if searchEnabled {
		service, err := websearch.New(websearch.Config{Auth: a.Auth, Client: a.SearchClient})
		if err != nil {
			return adapter.NewError(http.StatusServiceUnavailable, "web_search_unavailable", "ChatGPT web search is unavailable", err)
		}
		capture := newResponseCapture()
		httpRequest := httpRequestFrom(ctx, request)
		service.ServeResponses(capture, httpRequest, request.Body, websearch.BackendFunc(func(iterationCtx context.Context, iteration []byte) (*http.Response, error) {
			capture := newResponseCapture()
			iterationRequest := request
			iterationRequest.Body = iteration
			err := a.serveResponses(iterationCtx, iterationRequest, eventHTTPSink{capture})
			if err != nil {
				writeCapturedError(capture, err)
			}
			return capture.response()
		}))
		response, err := capture.response()
		if err != nil {
			return adapter.NewError(http.StatusBadGateway, "web_search_invalid_response", "web search returned an invalid response", err)
		}
		return capturedResponseEvents(response, sink)
	}
	return a.serveResponses(ctx, request, sink)
}

func (a Adapter) serveResponses(ctx context.Context, request adapter.Request, sink adapter.EventSink) error {
	translated, err := translateRequest(request.Body)
	if err != nil {
		if errors.Is(err, errEncryptedContextCompaction) || errors.Is(err, errResponsesCompaction) {
			return adapter.NewError(http.StatusUnprocessableEntity, "adapter_compaction_unsupported", err.Error(), err)
		}
		return adapter.NewError(http.StatusBadRequest, "invalid_request", err.Error(), err)
	}
	token, err := a.token()
	if err != nil {
		return adapter.NewError(http.StatusServiceUnavailable, "modal_credential_unavailable", err.Error(), err)
	}
	endpoint, err := a.endpoint()
	if err != nil {
		return adapter.NewError(http.StatusInternalServerError, "modal_configuration_error", err.Error(), err)
	}

	client := a.Client
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
		}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}

	var response *http.Response
	var lastStatus int
	sessionID := modalSessionID(request, translated.Model)
	for attempt := 0; attempt < 2; attempt++ {
		upstreamRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(translated.Body))
		if requestErr != nil {
			return adapter.NewError(http.StatusInternalServerError, "modal_configuration_error", "could not build Modal request", requestErr)
		}
		upstreamRequest.Header.Set("Authorization", "Bearer "+token)
		upstreamRequest.Header.Set("Content-Type", "application/json")
		upstreamRequest.Header.Set("Accept", "text/event-stream")
		upstreamRequest.Header.Set("Modal-Session-Id", sessionID)

		response, err = client.Do(upstreamRequest)
		if err == nil && !retryableStatus(response.StatusCode) {
			break
		}
		if response != nil {
			lastStatus = response.StatusCode
			_, _ = io.CopyN(io.Discard, response.Body, 64<<10)
			_ = response.Body.Close()
			response = nil
		}
		if attempt == 0 {
			if waitErr := waitForRetry(ctx, 250*time.Millisecond); waitErr != nil {
				err = waitErr
				break
			}
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return adapter.NewError(http.StatusGatewayTimeout, "modal_transport_error", "Modal request was canceled or timed out", err)
		}
		return adapter.NewError(http.StatusBadGateway, "modal_transport_error", "Modal could not be reached", err)
	}
	if response == nil {
		if lastStatus != 0 {
			return adapter.NewError(http.StatusBadGateway, "modal_upstream_error", fmt.Sprintf("Modal returned HTTP %d", lastStatus), nil)
		}
		return adapter.NewError(http.StatusBadGateway, "modal_transport_error", "Modal did not return a response", nil)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return adapter.NewError(http.StatusBadGateway, "modal_upstream_error", fmt.Sprintf("Modal returned HTTP %d", response.StatusCode), nil)
	}

	bridge := newResponseBridge(sink, translated)
	if err := bridgeChatStream(response.Body, bridge); err != nil {
		bridge.fail(err.Error())
		return nil
	}
	return nil
}

func (a Adapter) endpoint() (string, error) {
	base := strings.TrimRight(a.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid Modal base URL")
	}
	if parsed.Scheme != "https" {
		host := parsed.Hostname()
		if parsed.Scheme != "http" || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			return "", errors.New("Modal base URL must use HTTPS")
		}
	}
	return base + "/chat/completions", nil
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusConflict || status == http.StatusTooManyRequests || status >= 500
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func modalSessionID(request adapter.Request, model string) string {
	seed := model
	found := false
	for _, key := range []string{"thread-id", "x-codex-parent-thread-id", "conversation-id", "session-id"} {
		if value := strings.TrimSpace(request.MetadataValue(key)); value != "" {
			seed += "\x00" + key + "=" + value
			found = true
		}
	}
	if !found {
		return identifier("codex-")
	}
	digest := sha256.Sum256([]byte(seed))
	return "codex-" + hex.EncodeToString(digest[:16])
}
