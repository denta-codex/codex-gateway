package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denta-codex/codex-gateway/adapter"
	"github.com/denta-codex/codex-gateway/adapters/example"
)

func TestMergeAndKeepLastGoodCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	native := []byte(`{"models":[{"slug":"gpt-6-sol","unknown_future_field":{"preserve":true}}]}`)
	if err := Refresh(context.Background(), path, func(context.Context) ([]byte, error) { return native, nil }, []adapter.Adapter{example.Adapter{}}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "unknown_future_field") || !strings.Contains(string(first), "example/echo") {
		t.Fatalf("catalog=%s", first)
	}
	if err := Refresh(context.Background(), path, func(context.Context) ([]byte, error) { return nil, errors.New("offline") }, []adapter.Adapter{example.Adapter{}}); err == nil {
		t.Fatal("expected refresh error")
	}
	last, _ := os.ReadFile(path)
	if string(last) != string(first) {
		t.Fatal("failed refresh replaced last good catalog")
	}
}
