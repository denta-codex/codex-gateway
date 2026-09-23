package subscription

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func jwtAt(exp time.Time) string {
	data, _ := json.Marshal(map[string]int64{"exp": exp.Unix()})
	return "a." + base64.RawURLEncoding.EncodeToString(data) + ".b"
}

func TestRefreshReadsCodexUpdatedCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	write := func(token string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"access_token": token, "account_id": "account"}})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(jwtAt(time.Now().Add(time.Minute)))
	count := 0
	auth := &Auth{Path: path, Refresh: func(context.Context) error { count++; write(jwtAt(time.Now().Add(time.Hour))); return nil }}
	first, account, err := auth.Token(context.Background())
	if err != nil || account != "account" || first == "" || count != 1 {
		t.Fatalf("first token account=%q refresh=%d err=%v", account, count, err)
	}
	_, _, err = auth.Token(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("unnecessary refresh count=%d err=%v", count, err)
	}
}

func TestRefreshUsesCodexProactiveAccountRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-codex")
	script := `#!/bin/sh
IFS= read -r initialize || exit 1
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r initialized || exit 1
IFS= read -r account || exit 1
case "$account" in
  *'"method":"account/read"'*'"refreshToken":true'*) printf '%s\n' '{"id":2,"result":{"account":null}}' ;;
  *) printf '%s\n' '{"id":2,"error":{"message":"wrong refresh request"}}' ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	auth := &Auth{CodexBinary: path}
	if err := auth.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}
