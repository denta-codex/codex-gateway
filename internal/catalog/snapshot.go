package catalog

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
)

// Snapshot is the exact catalog document served to Codex and its strong ETag.
type Snapshot struct {
	Data []byte
	ETag string
}

func ReadSnapshot(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	sum := sha256.Sum256(data)
	return Snapshot{Data: data, ETag: fmt.Sprintf(`"sha256-%x"`, sum)}, nil
}

// IfNoneMatch reports whether an If-None-Match value weakly matches etag.
func IfNoneMatch(value, etag string) bool {
	for candidate := range strings.SplitSeq(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
