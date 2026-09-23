package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denta-codex/codex-gateway/adapter"
	"github.com/denta-codex/codex-gateway/adapters/chatgpt"
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

func TestChatGPTUsesNativeSchemaWithoutNativeCapabilities(t *testing.T) {
	native := []byte(`{"models":[{"slug":"native","visibility":"list","priority":1,"base_instructions":"native instructions","model_messages":{"instructions_template":"native model"},"supports_search_tool":true,"web_search_tool_type":"text_and_image","availability_nux":{"message":"native promotion"},"future_schema_field":{"keep":true}}]}`)
	merged, err := Merge(native, []adapter.Adapter{&chatgpt.Adapter{}})
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(merged, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 {
		t.Fatalf("models=%d", len(catalog.Models))
	}
	row := catalog.Models[1]
	var future struct {
		Keep bool `json:"keep"`
	}
	if err := json.Unmarshal(row["future_schema_field"], &future); err != nil {
		t.Fatal(err)
	}
	if _, ok := row["web_search_tool_type"]; ok {
		t.Fatal("native web search capability leaked")
	}
	if _, ok := row["availability_nux"]; ok {
		t.Fatal("native promotion leaked")
	}
	if string(row["supports_search_tool"]) != "false" || string(row["priority"]) != "100" || !future.Keep {
		t.Fatalf("merged row: %s", row)
	}
	if strings.Contains(string(row["base_instructions"]), "native instructions") || strings.Contains(string(row["model_messages"]), "native model") {
		t.Fatal("native model instructions leaked")
	}
}
