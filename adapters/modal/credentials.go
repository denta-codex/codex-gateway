package modal

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const tokenCredentialName = "modal-inference-token"

func (a Adapter) token() (string, error) {
	if strings.TrimSpace(a.Token) != "" {
		return strings.TrimSpace(a.Token), nil
	}
	paths := make([]string, 0, 4)
	if a.CredentialFile != "" {
		paths = append(paths, a.CredentialFile)
	}
	if path := os.Getenv("MODAL_INFERENCE_TOKEN_FILE"); path != "" {
		paths = append(paths, path)
	}
	systemdCredentialPath := ""
	if directory := os.Getenv("CREDENTIALS_DIRECTORY"); directory != "" {
		systemdCredentialPath = filepath.Join(directory, tokenCredentialName)
		paths = append(paths, systemdCredentialPath)
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths,
			filepath.Join(home, ".config/codex-gateway/modal.env"),
			filepath.Join(home, ".config/codex-modal-proxy/credentials.env"),
		)
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read Modal credential: %w", err)
		}
		permissions := info.Mode().Perm()
		private := permissions&0o077 == 0
		if path == systemdCredentialPath {
			// systemd credentials are exposed from a protected, service-private
			// mount as 0440. Group-read is intentional there; group write/execute
			// and every permission for other users remain forbidden.
			private = permissions&0o037 == 0
		}
		if !info.Mode().IsRegular() || !private {
			return "", errors.New("read Modal credential: credential file must be private and regular")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read Modal credential: %w", err)
		}
		token, err := parseCredential(data)
		if err != nil {
			return "", fmt.Errorf("read Modal credential: %w", err)
		}
		return token, nil
	}
	return "", errors.New("Modal proxy credential unavailable")
}

func parseCredential(data []byte) (string, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", errors.New("credential file is empty")
	}
	if !strings.Contains(text, "=") && !strings.ContainsAny(text, "\r\n") {
		return text, nil
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		values[strings.TrimSpace(key)] = value
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if token := strings.TrimSpace(values["MODAL_PROXY_TOKEN"]); token != "" {
		return token, nil
	}
	wk, ws := strings.TrimSpace(values["WK_SECRET"]), strings.TrimSpace(values["WS_SECRET"])
	if wk == "" || ws == "" {
		return "", errors.New("credential file needs MODAL_PROXY_TOKEN or WK_SECRET and WS_SECRET")
	}
	return wk + "." + ws, nil
}
