package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSnapshotAndIfNoneMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{"models":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.ETag == "" || !IfNoneMatch(first.ETag, first.ETag) || !IfNoneMatch("W/"+first.ETag, first.ETag) {
		t.Fatalf("etag=%q", first.ETag)
	}
	if err := os.WriteFile(path, []byte(`{"models":[{"slug":"new"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.ETag == first.ETag || IfNoneMatch(first.ETag, second.ETag) {
		t.Fatalf("first=%q second=%q", first.ETag, second.ETag)
	}
}
