package modal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCredential(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		input string
		want  string
	}{
		"direct":      {"wk.ws\n", "wk.ws"},
		"proxy token": {"MODAL_PROXY_TOKEN='wk.ws'\n", "wk.ws"},
		"pair":        {"export WK_SECRET=wk\nWS_SECRET=ws\n", "wk.ws"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := parseCredential([]byte(test.input))
			if err != nil || got != test.want {
				t.Fatalf("parseCredential() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestParseCredentialRejectsPartialPair(t *testing.T) {
	t.Parallel()
	if _, err := parseCredential([]byte("WK_SECRET=wk\n")); err == nil {
		t.Fatal("parseCredential accepted an incomplete credential")
	}
}

func TestTokenRequiresPrivateFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "modal.env")
	if err := os.WriteFile(path, []byte("MODAL_PROXY_TOKEN=wk.ws\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Adapter{CredentialFile: path}).token(); err == nil {
		t.Fatal("accepted a group- or world-readable credential")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := (Adapter{CredentialFile: path}).token(); err != nil || got != "wk.ws" {
		t.Fatalf("private credential = %q, %v", got, err)
	}
}

func TestTokenAcceptsSystemdCredentialMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, tokenCredentialName)
	if err := os.WriteFile(path, []byte("wk.ws\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o440); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", directory)
	if got, err := (Adapter{}).token(); err != nil || got != "wk.ws" {
		t.Fatalf("systemd credential = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := (Adapter{}).token(); err == nil {
		t.Fatal("accepted a world-readable systemd credential")
	}
}
