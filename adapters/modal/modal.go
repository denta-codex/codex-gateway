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

func (a Adapter) ServeResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	searchEnabled, _ := websearch.Enabled(body)
	if searchEnabled {
		service, err := websearch.New(websearch.Config{Auth: a.Auth, Client: a.SearchClient})
		if err != nil {
			respondAdapterError(w, http.StatusServiceUnavailable, "web_search_unavailable", "ChatGPT web search is unavailable")
			return
		}
		service.ServeResponses(w, r, body, websearch.BackendFunc(func(ctx context.Context, iteration []byte) (*http.Response, error) {
			capture := newResponseCapture()
			a.serveResponses(capture, r.Clone(ctx), iteration)
			return capture.response()
		}))
		return
	}
	a.serveResponses(w, r, body)
}

func (a Adapter) serveResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	translated, err := translateRequest(body)
	if err != nil {
		respondAdapterError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	token, err := a.token()
	if err != nil {
		respondAdapterError(w, http.StatusServiceUnavailable, "modal_credential_unavailable", err.Error())
		return
	}
	endpoint, err := a.endpoint()
	if err != nil {
		respondAdapterError(w, http.StatusInternalServerError, "modal_configuration_error", err.Error())
		return
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
	sessionID := modalSessionID(r, translated.Model)
	for attempt := 0; attempt < 2; attempt++ {
		request, requestErr := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(translated.Body))
		if requestErr != nil {
			respondAdapterError(w, http.StatusInternalServerError, "modal_configuration_error", "could not build Modal request")
			return
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream")
		request.Header.Set("Modal-Session-Id", sessionID)

		response, err = client.Do(request)
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
			if waitErr := waitForRetry(r.Context(), 250*time.Millisecond); waitErr != nil {
				err = waitErr
				break
			}
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			respondAdapterError(w, http.StatusGatewayTimeout, "modal_transport_error", "Modal request was canceled or timed out")
			return
		}
		respondAdapterError(w, http.StatusBadGateway, "modal_transport_error", "Modal could not be reached")
		return
	}
	if response == nil {
		if lastStatus != 0 {
			respondAdapterError(w, http.StatusBadGateway, "modal_upstream_error", fmt.Sprintf("Modal returned HTTP %d", lastStatus))
			return
		}
		respondAdapterError(w, http.StatusBadGateway, "modal_transport_error", "Modal did not return a response")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		respondAdapterError(w, http.StatusBadGateway, "modal_upstream_error", fmt.Sprintf("Modal returned HTTP %d", response.StatusCode))
		return
	}

	bridge := newResponseBridge(w, translated)
	if err := bridgeChatStream(response.Body, bridge); err != nil {
		bridge.fail(err.Error())
	}
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

func modalSessionID(r *http.Request, model string) string {
	seed := model
	found := false
	for _, key := range []string{"thread-id", "x-codex-parent-thread-id", "conversation-id", "session-id"} {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
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

func respondAdapterError(w http.ResponseWriter, status int, code, message string) {
	_ = writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": message, "type": "adapter_error",
	}})
}
