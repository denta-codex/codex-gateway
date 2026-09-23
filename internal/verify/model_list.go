package verify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ModelList starts a fresh Codex app server and requires it to expose every
// model in the candidate catalog. This is a deployment gate, not a health probe.
func ModelList(parent context.Context, codexBinary, catalogPath, upstream string) error {
	raw, err := os.ReadFile(catalogPath)
	if err != nil {
		return err
	}
	var catalog struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
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
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBinary,
		"-c", "model_catalog_json="+fmt.Sprintf("%q", catalogPath),
		"-c", "openai_base_url="+fmt.Sprintf("%q", upstream),
		"app-server", "--stdio")
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
	return nil
}
