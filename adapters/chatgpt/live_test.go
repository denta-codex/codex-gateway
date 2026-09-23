package chatgpt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denta-codex/codex-gateway/internal/subscription"
)

// This opt-in test exercises the undocumented consumer conversation endpoint.
// It reads the existing Grace login without rotating or persisting credentials.
func TestLiveInstantInference(t *testing.T) {
	if os.Getenv("CODEX_GATEWAY_LIVE_CHATGPT") != "1" {
		t.Skip("set CODEX_GATEWAY_LIVE_CHATGPT=1 for live inference")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	auth := &subscription.Auth{Path: filepath.Join(home, ".codex", "auth.json")}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	token, account, err := auth.ReadOnlyToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	err = processRunner(ctx, transportInput{AccessToken: token, AccountID: account, Lane: "instant", Prompt: "Reply with exactly GATEWAY_CHATGPT_OK"}, func(text string) error { _, err := output.WriteString(text); return err })
	if err != nil {
		t.Fatalf("live transport: %v", err)
	}
	if strings.TrimSpace(output.String()) != "GATEWAY_CHATGPT_OK" {
		t.Fatalf("unexpected ChatGPT output: %q", output.String())
	}
}
