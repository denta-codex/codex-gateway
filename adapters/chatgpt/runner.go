package chatgpt

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/denta-codex/codex-gateway/adapter"
)

//go:embed proof.py stream.py transport.py
var scripts embed.FS

type transportInput struct {
	AccessToken string `json:"accessToken"`
	AccountID   string `json:"accountId"`
	Lane        string `json:"lane"`
	Effort      string `json:"effort,omitempty"`
	Prompt      string `json:"prompt"`
}

type Runner func(context.Context, transportInput, func(string) error) error

type transportError struct {
	Code       string
	Status     int
	Dispatched bool
}

func (e *transportError) Error() string { return "ChatGPT transport: " + e.Code }

var safeErrorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// processRunner keeps credentials on stdin. The helper announces readiness
// before the single inference send, so setup failures cannot cause a replay.
func processRunner(ctx context.Context, input transportInput, onText func(string) error) error {
	ctx, cancel := context.WithTimeout(ctx, 16*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "codex-gateway-chatgpt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, name := range []string{"proof.py", "stream.py", "transport.py"} {
		contents, err := scripts.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), contents, 0600); err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(ctx, "uv", "run", "--offline", "--no-project", "--with", "curl_cffi==0.16.3", "--with", "websocket-client==1.9.2", "python", filepath.Join(dir, "transport.py"))
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	waited := false
	defer func() {
		_ = stdin.Close()
		if !waited && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if !waited {
			_ = cmd.Wait()
		}
	}()
	if err := json.NewEncoder(stdin).Encode(input); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxPayload+1024)
	ready, dispatched, done, outputBytes := false, false, false, 0
	for scanner.Scan() {
		var event struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Code   string `json:"code"`
			Status int    `json:"status"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return &transportError{"invalid_stream", 0, dispatched}
		}
		switch event.Type {
		case "ready":
			if ready {
				return &transportError{"duplicate_dispatch", 0, dispatched}
			}
			ready = true
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := io.WriteString(stdin, "send\n"); err != nil {
				return err
			}
			if err := stdin.Close(); err != nil {
				return err
			}
			dispatched = true
		case "text":
			if !dispatched || done {
				return &transportError{"invalid_stream", 0, dispatched}
			}
			outputBytes += len(event.Text)
			if outputBytes > maxPayload {
				return &transportError{"output_limit", 0, true}
			}
			if err := onText(event.Text); err != nil {
				return err
			}
		case "done":
			if !dispatched || done {
				return &transportError{"invalid_stream", 0, dispatched}
			}
			done = true
		case "error":
			if !safeErrorCode.MatchString(event.Code) {
				event.Code = "transport_failure"
			}
			return &transportError{event.Code, event.Status, dispatched}
		case "heartbeat", "catalog":
		default:
			return &transportError{"invalid_stream", 0, dispatched}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("ChatGPT helper stream: %w", err)
	}
	if !dispatched || !done || outputBytes == 0 {
		return &transportError{"incomplete_stream", 0, dispatched}
	}
	waitErr := cmd.Wait()
	waited = true
	if waitErr != nil {
		return &transportError{"helper_exited", 0, dispatched}
	}
	return nil
}

func runWithRefresh(ctx context.Context, source adapter.TokenSource, runner Runner, p prepared, onText func(string) error) error {
	if source == nil {
		return errors.New("ChatGPT login unavailable")
	}
	if runner == nil {
		runner = processRunner
	}
	token, account, err := source.Token(ctx)
	if err != nil {
		return errors.New("Grace needs codex login")
	}
	input := transportInput{AccessToken: token, AccountID: account, Lane: p.mode.lane, Effort: p.mode.effort, Prompt: p.prompt}
	err = runner(ctx, input, onText)
	// Refresh is allowed only before an inference send. A dispatched request is
	// never replayed because its completion state may be unknown.
	var te *transportError
	if errors.As(err, &te) && te.Status == 401 && !te.Dispatched {
		if refresher, ok := source.(interface {
			RefreshIfUnchanged(context.Context, string) error
		}); ok {
			if refreshErr := refresher.RefreshIfUnchanged(ctx, token); refreshErr != nil {
				return errors.New("Grace needs codex login")
			}
			token, account, err = source.Token(ctx)
			if err != nil {
				return errors.New("Grace needs codex login")
			}
			input.AccessToken, input.AccountID = token, account
			return runner(ctx, input, onText)
		}
	}
	return err
}

func publicTransportError(err error) (status int, message string) {
	if err == nil {
		return 200, ""
	}
	if errors.Is(err, context.Canceled) {
		return 499, "ChatGPT request canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 504, "ChatGPT request timed out"
	}
	var te *transportError
	if errors.As(err, &te) {
		switch te.Code {
		case "mode_unavailable", "effort_unavailable":
			return 400, "Selected ChatGPT mode is unavailable for this account"
		case "login_required":
			return 503, "Grace needs codex login"
		case "empty_response", "incomplete_stream":
			return 502, "ChatGPT response was incomplete"
		default:
			return 502, "ChatGPT conversation transport failed"
		}
	}
	if strings.Contains(err.Error(), "codex login") {
		return 503, "Grace needs codex login"
	}
	return 502, "ChatGPT conversation failed"
}
