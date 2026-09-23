package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/denta-codex/codex-gateway/adapter"
)

type Source func(context.Context) ([]byte, error)
type TokenSource interface {
	Token(context.Context) (string, string, error)
}

func SubscriptionSource(auth TokenSource, client *http.Client, upstream, codexBinary string) Source {
	return func(ctx context.Context) ([]byte, error) {
		if codexBinary == "" {
			codexBinary = "codex"
		}
		versionOutput, err := exec.CommandContext(ctx, codexBinary, "--version").Output()
		if err != nil {
			return nil, fmt.Errorf("read Codex version: %w", err)
		}
		version := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(versionOutput)), "codex-cli "))
		if version == "" || strings.ContainsAny(version, " ?&#/") {
			return nil, errors.New("invalid Codex client version")
		}
		token, account, err := auth.Token(ctx)
		if err != nil {
			return nil, err
		}
		target, err := url.Parse(strings.TrimRight(upstream, "/") + "/models")
		if err != nil || target.Scheme != "https" {
			return nil, errors.New("catalog upstream must be HTTPS")
		}
		query := target.Query()
		query.Set("client_version", version)
		target.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Chatgpt-Account-Id", account)
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("read Grace subscription models: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return nil, fmt.Errorf("subscription models returned HTTP %d", response.StatusCode)
		}
		return io.ReadAll(io.LimitReader(response.Body, 8<<20))
	}
}

func Merge(native []byte, adapters []adapter.Adapter) ([]byte, error) {
	var document struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(native, &document); err != nil {
		return nil, err
	}
	if len(document.Models) == 0 {
		return nil, errors.New("empty subscription model catalog")
	}
	seen := map[string]bool{}
	var nativeTemplate map[string]json.RawMessage
	for _, row := range document.Models {
		var item struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		}
		if json.Unmarshal(row, &item) != nil || item.Slug == "" || seen[item.Slug] {
			return nil, errors.New("invalid subscription model catalog")
		}
		seen[item.Slug] = true
		if nativeTemplate == nil && item.Visibility != "hide" {
			if err := json.Unmarshal(row, &nativeTemplate); err != nil {
				return nil, err
			}
		}
	}
	for _, extension := range adapters {
		namespace := extension.Namespace()
		if namespace == "" || strings.ContainsAny(namespace, "/ \\:") {
			return nil, fmt.Errorf("invalid adapter namespace %q", namespace)
		}
		for _, model := range extension.Models() {
			if !strings.HasPrefix(model.Slug, namespace+"/") || seen[model.Slug] {
				return nil, fmt.Errorf("invalid or duplicate adapter model %q", model.Slug)
			}
			var overrides map[string]json.RawMessage
			if json.Unmarshal(model.Catalog, &overrides) != nil {
				return nil, fmt.Errorf("invalid catalog row for %q", model.Slug)
			}
			var slug string
			if json.Unmarshal(overrides["slug"], &slug) != nil || slug != model.Slug {
				return nil, fmt.Errorf("invalid catalog row for %q", model.Slug)
			}
			row := model.Catalog
			if model.TemplateNative {
				if nativeTemplate == nil {
					return nil, errors.New("catalog has no listable native model template")
				}
				merged := make(map[string]json.RawMessage, len(nativeTemplate)+len(overrides))
				for key, value := range nativeTemplate {
					merged[key] = value
				}
				for key, value := range overrides {
					if string(value) == "null" {
						delete(merged, key)
					} else {
						merged[key] = value
					}
				}
				var err error
				row, err = json.Marshal(merged)
				if err != nil {
					return nil, err
				}
			}
			seen[model.Slug] = true
			document.Models = append(document.Models, row)
		}
	}
	return json.MarshalIndent(document, "", "  ")
}

// Refresh replaces the catalog only after a complete successful merge.
func Refresh(ctx context.Context, path string, source Source, adapters []adapter.Adapter) error {
	native, err := source(ctx)
	if err != nil {
		return err
	}
	merged, err := Merge(native, adapters)
	if err != nil {
		return err
	}
	merged = append(merged, '\n')
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, merged) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".catalog-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(merged); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
