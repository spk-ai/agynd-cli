package daemon

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteCodexAuthShape(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := writeCodexAuth(time.Date(2026, 8, 9, 16, 59, 35, 0, time.UTC)); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var auth codexAuth
	if err := json.Unmarshal(data, &auth); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if auth.AuthMode != "chatgpt" {
		t.Fatalf("auth_mode = %q", auth.AuthMode)
	}
	if auth.OpenAIAPIKey != nil {
		t.Fatalf("OPENAI_API_KEY = %v, want null", *auth.OpenAIAPIKey)
	}
	if auth.Tokens.IDToken == "" {
		t.Fatal("id_token is empty")
	}
	// The proxy builds the upstream header from the resolved subscription, so a
	// credential here would be dead weight the container has no business holding.
	if auth.Tokens.AccessToken != "" || auth.Tokens.RefreshToken != "" || auth.Tokens.AccountID != "" {
		t.Fatalf("credential fields not blank: %+v", auth.Tokens)
	}
	if auth.LastRefresh != "2026-08-09T16:59:35Z" {
		t.Fatalf("last_refresh = %q", auth.LastRefresh)
	}
}

// codex decodes the token before it reaches the network, so it has to be a real
// JWT -- three segments, and a claim set that parses.
func TestCodexPlaceholderIDTokenDecodes(t *testing.T) {
	segments := strings.Split(codexPlaceholderIDToken, ".")
	if len(segments) != 3 {
		t.Fatalf("segments = %d, want 3", len(segments))
	}
	for _, segment := range segments[:2] {
		decoded, err := base64.RawURLEncoding.DecodeString(segment)
		if err != nil {
			t.Fatalf("decode %q: %v", segment, err)
		}
		var claims map[string]any
		if err := json.Unmarshal(decoded, &claims); err != nil {
			t.Fatalf("parse %s: %v", decoded, err)
		}
	}
}

// A real credential may already be there, from the CLI's own login or a previous
// session, and replacing it with a blank one logs the engineer out.
func TestWriteCodexAuthLeavesAnExistingFileAlone(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(path, []byte("the engineer's own token"), 0o600); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if err := writeCodexAuth(time.Now()); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "the engineer's own token" {
		t.Fatalf("overwrote an existing credential: %s", data)
	}
}
