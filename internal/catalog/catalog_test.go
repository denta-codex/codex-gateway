package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/denta-codex/codex-gateway/adapter"
	"github.com/denta-codex/codex-gateway/adapters/chatgpt"
	"github.com/denta-codex/codex-gateway/adapters/example"
	"github.com/denta-codex/codex-gateway/adapters/modal"
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

func TestModalUsesNativeSchemaWithoutNativeIdentity(t *testing.T) {
	native := []byte(`{"models":[{"slug":"native","visibility":"list","priority":1,"base_instructions":"You are NativeGPT","model_messages":{"instructions_template":"native-only"},"supports_search_tool":true,"web_search_tool_type":"text_and_image","future_schema_field":{"keep":true}}]}`)
	merged, err := Merge(native, []adapter.Adapter{modal.Adapter{}})
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(merged, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1+len(modal.Adapter{}.Models()) {
		t.Fatalf("models=%d", len(catalog.Models))
	}
	for _, row := range catalog.Models[1:] {
		if strings.Contains(string(row["base_instructions"]), "NativeGPT") || strings.Contains(string(row["model_messages"]), "native-only") {
			t.Fatal("native model identity leaked into Modal catalog row")
		}
		if string(row["web_search_tool_type"]) != `"text_and_image"` || string(row["supports_search_tool"]) != "true" {
			t.Fatal("Modal hosted search capability was not applied explicitly")
		}
		if !strings.Contains(string(row["future_schema_field"]), "keep") {
			t.Fatal("future native schema field was not retained")
		}
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
	if string(row["supports_search_tool"]) != "false" || !future.Keep {
		t.Fatalf("merged row: %s", row)
	}
	if strings.Contains(string(row["base_instructions"]), "native instructions") || strings.Contains(string(row["model_messages"]), "native model") {
		t.Fatal("native model instructions leaked")
	}
}

func TestSubagentCatalogPolicy(t *testing.T) {
	native := []byte(`{"models":[
		{"slug":"gpt-6-astra","visibility":"list","priority":1},
		{"slug":"gpt-6-sol","visibility":"list","priority":2},
		{"slug":"gpt-6-luna","visibility":"list","priority":3},
		{"slug":"gpt-5.6-sol","visibility":"list","priority":4}
	]}`)
	merged, err := Merge(native, []adapter.Adapter{modal.Adapter{}})
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []struct {
			Slug              string `json:"slug"`
			Priority          int    `json:"priority"`
			MultiAgentVersion string `json:"multi_agent_version"`
		} `json:"models"`
	}
	if err := json.Unmarshal(merged, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.MultiAgentVersion != "v1" {
			t.Fatalf("model %q collaboration version=%q", model.Slug, model.MultiAgentVersion)
		}
	}
	sort.Slice(catalog.Models, func(i, j int) bool { return catalog.Models[i].Priority < catalog.Models[j].Priority })
	if len(catalog.Models) < len(advertisedSubagentModels) {
		t.Fatalf("models=%d", len(catalog.Models))
	}
	for index, slug := range advertisedSubagentModels {
		if catalog.Models[index].Slug != slug || catalog.Models[index].Priority != index {
			t.Fatalf("roster[%d]=%q priority=%d", index, catalog.Models[index].Slug, catalog.Models[index].Priority)
		}
	}
	if catalog.Models[len(advertisedSubagentModels)].Slug != "gpt-6-astra" {
		t.Fatalf("first non-roster model=%q", catalog.Models[len(advertisedSubagentModels)].Slug)
	}
}
