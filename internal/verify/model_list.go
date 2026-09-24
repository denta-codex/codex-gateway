package verify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/denta-codex/codex-gateway/internal/catalog"
)

// ModelList starts a fresh Codex app server and requires it to expose every
// model in the candidate catalog after discovering it through /v1/models. This
// is a deployment gate, not a health probe.
func ModelList(parent context.Context, codexBinary, catalogPath, authPath, discoveryBaseURL string) error {
	snapshot, err := catalog.ReadSnapshot(catalogPath)
	if err != nil {
		return err
	}
	var catalog struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(snapshot.Data, &catalog); err != nil {
		return err
	}
	if len(catalog.Models) == 0 {
		return errors.New("candidate catalog is empty")
	}
	expected := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		if model.Slug == "" {
			return errors.New("candidate catalog contains an empty slug")
		}
		if model.Visibility == "hide" {
			continue
		}
		expected[model.Slug] = false
	}
	if len(expected) == 0 {
		return errors.New("candidate catalog has no listable models")
	}
	var requested atomic.Bool
	var server *httptest.Server
	if discoveryBaseURL == "" {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("start verification model endpoint: %w", err)
		}
		server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
				http.NotFound(w, r)
				return
			}
			requested.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", snapshot.ETag)
			_, _ = w.Write(snapshot.Data)
		}))
		server.Listener = listener
		server.Start()
		defer server.Close()
		discoveryBaseURL = server.URL + "/v1"
	} else if err := validateLoopbackBaseURL(discoveryBaseURL); err != nil {
		return err
	}
	auth, err := os.ReadFile(authPath)
	if err != nil {
		return fmt.Errorf("read verification auth: %w", err)
	}
	base := os.Getenv("CODEX_HOME")
	if base == "" {
		base = os.TempDir()
	}
	verificationHome, err := os.MkdirTemp(base, "model-list-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(verificationHome)
	if err := os.WriteFile(filepath.Join(verificationHome, "auth.json"), auth, 0600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBinary,
		"-c", "openai_base_url="+fmt.Sprintf("%q", discoveryBaseURL),
		"-c", `model_provider="openai"`,
		"app-server", "--stdio")
	cmd.Env = append(envWithoutCodexHome(os.Environ()), "CODEX_HOME="+verificationHome)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// App-server diagnostics stay out of deployment logs; they may contain paths.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	request := func(value any) error { return json.NewEncoder(stdin).Encode(value) }
	if err := request(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "codex-gateway-verify", "version": "0.1"}, "capabilities": map[string]any{}}}); err != nil {
		return err
	}
	lines := make(chan []byte, 16)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), 16<<20)
		for scanner.Scan() {
			lines <- append([]byte(nil), scanner.Bytes()...)
		}
		close(lines)
	}()
	response := func(id int) (json.RawMessage, error) {
		for {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("Codex model/list timed out: %w", ctx.Err())
			case line, ok := <-lines:
				if !ok {
					return nil, errors.New("Codex app server exited before model/list")
				}
				var message struct {
					ID     int             `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  json.RawMessage `json:"error"`
				}
				if json.Unmarshal(line, &message) != nil || message.ID != id {
					continue
				}
				if len(message.Error) > 0 {
					return nil, fmt.Errorf("Codex app server rejected request %d: %s", id, string(message.Error))
				}
				return message.Result, nil
			}
		}
	}
	if _, err := response(1); err != nil {
		return err
	}
	if err := request(map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	if err := request(map[string]any{"id": 2, "method": "model/list", "params": map[string]any{}}); err != nil {
		return err
	}
	result, err := response(2)
	if err != nil {
		return err
	}
	var listed struct {
		Data []struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Slug  string `json:"slug"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result, &listed); err != nil {
		return err
	}
	if len(listed.Data) == 0 {
		return errors.New("fresh Codex model/list was empty")
	}
	for _, model := range listed.Data {
		for _, key := range []string{model.ID, model.Model, model.Slug} {
			if _, ok := expected[key]; ok {
				expected[key] = true
			}
		}
	}
	missing := []string{}
	for slug, found := range expected {
		if !found {
			missing = append(missing, slug)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("fresh Codex model/list omitted catalog models: %s", strings.Join(missing, ", "))
	}
	if server != nil && !requested.Load() {
		return errors.New("fresh Codex model/list did not request /v1/models")
	}
	return nil
}

func validateLoopbackBaseURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("discovery base URL must be a fixed HTTP URL on 127.0.0.1")
	}
	return nil
}

func envWithoutCodexHome(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "CODEX_HOME=") {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
