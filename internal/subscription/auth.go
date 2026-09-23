package subscription

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type credentials struct {
	Mode   string `json:"auth_mode"`
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// Auth reads Grace's existing Codex login on every request, so a normal Codex
// login or token refresh becomes visible without restarting the gateway.
type Auth struct {
	Path        string
	CodexBinary string
	mu          sync.Mutex
	Refresh     func(context.Context) error // optional test seam
}

func (a *Auth) read() (credentials, error) {
	var result credentials
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	if result.Mode != "chatgpt" || result.Tokens.AccessToken == "" || result.Tokens.AccountID == "" {
		return result, errors.New("Grace needs codex login with ChatGPT")
	}
	return result, nil
}

func expiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func (a *Auth) refresh(ctx context.Context) error {
	if a.Refresh != nil {
		return a.Refresh(ctx)
	}
	binary := a.CodexBinary
	if binary == "" {
		binary = "codex"
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(refreshCtx, binary, "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 32<<10), 1<<20)
	send := func(value any) error { return json.NewEncoder(stdin).Encode(value) }
	wait := func(id int) error {
		for scanner.Scan() {
			var response struct {
				ID    int             `json:"id"`
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &response) != nil || response.ID != id {
				continue
			}
			if len(response.Error) > 0 {
				return errors.New("Codex rejected credential refresh; Grace may need codex login")
			}
			return nil
		}
		if err := refreshCtx.Err(); err != nil {
			return fmt.Errorf("Codex credential refresh timed out: %w", err)
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("Codex credential refresh stopped: %w", err)
		}
		return errors.New("Codex credential refresh stopped before account/read")
	}
	if err := send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "codex-gateway", "version": "0.1"}, "capabilities": map[string]any{}}}); err != nil {
		return err
	}
	if err := wait(1); err != nil {
		return err
	}
	if err := send(map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	if err := send(map[string]any{"id": 2, "method": "account/read", "params": map[string]bool{"refreshToken": true}}); err != nil {
		return err
	}
	return wait(2)
}

func (a *Auth) Token(ctx context.Context) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, err := a.read()
	if err != nil {
		return "", "", err
	}
	if end := expiry(current.Tokens.AccessToken); !end.IsZero() && time.Until(end) < 5*time.Minute {
		if err := a.refresh(ctx); err != nil {
			return "", "", err
		}
		current, err = a.read()
		if err != nil {
			return "", "", err
		}
		if end := expiry(current.Tokens.AccessToken); !end.IsZero() && !end.After(time.Now()) {
			return "", "", errors.New("Grace ChatGPT token remains expired after Codex refresh")
		}
	}
	return current.Tokens.AccessToken, current.Tokens.AccountID, nil
}

// ReadOnlyToken is used by deployment previews. It never asks Codex to rotate
// credentials, so Ansible check mode cannot change Grace's login state.
func (a *Auth) ReadOnlyToken(context.Context) (string, string, error) {
	current, err := a.read()
	if err != nil {
		return "", "", err
	}
	if end := expiry(current.Tokens.AccessToken); !end.IsZero() && time.Until(end) < time.Minute {
		return "", "", errors.New("Grace ChatGPT token needs refresh before preview")
	}
	return current.Tokens.AccessToken, current.Tokens.AccountID, nil
}
