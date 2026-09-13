package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agynio/agynd-cli/internal/codexbridge"
	"github.com/agynio/agynd-cli/internal/config"
)

func TestCodexStateHome(t *testing.T) {
	t.Setenv("HOME", "/home/agent")
	for _, tc := range []struct{ name, input, want string }{
		{"default", "", "/home/agent/.codex"},
		{"override", "/state/codex", "/state/codex"},
		{"clean", "/state/codex/../codex", "/state/codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", tc.input)
			got, err := codexStateHome()
			if err != nil || got != tc.want {
				t.Fatalf("state home = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Setenv("CODEX_HOME", "relative/path")
	if _, err := codexStateHome(); err == nil {
		t.Fatal("relative CODEX_HOME accepted")
	}
}

func TestCodexRelocatedStateSurvivesHomeReplacement(t *testing.T) {
	state := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", state)
	originalSystemConfig := codexSystemConfigPath
	codexSystemConfigPath = filepath.Join(t.TempDir(), "etc", "codex", "config.toml")
	t.Cleanup(func() { codexSystemConfigPath = originalSystemConfig })

	if err := writeCodexAuth(time.Now()); err != nil {
		t.Fatal(err)
	}
	codexHome, err := writeCodexConfig(config.Config{LLMNative: true})
	if err != nil || codexHome != state {
		t.Fatalf("write config = %q, %v", codexHome, err)
	}
	sessionPath := filepath.Join(state, "sessions", "retained.jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, []byte("session history"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := codexbridge.ThreadMappingRecord{InstanceID: "instance-1", CodexThreadID: "session-1", CreatedAtUnixMs: 1, LastUsedAtUnixMs: 1}
	if err := codexMappingStore(codexHome, home).Save(record); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(state, "auth.json")
	if err := os.WriteFile(authPath, []byte("existing credential"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A new pod has a different ephemeral HOME but the same mounted state.
	replacementHome := t.TempDir()
	t.Setenv("HOME", replacementHome)
	if err := prepareAgentCLI(config.Config{Mode: config.ModeHolder, SDK: SDKCodex, LLMNative: true}); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := codexMappingStore(state, replacementHome).Load(record.InstanceID)
	if err != nil || !ok || loaded != record {
		t.Fatalf("mapping = %+v, %v, %v", loaded, ok, err)
	}
	if data, err := os.ReadFile(sessionPath); err != nil || string(data) != "session history" {
		t.Fatalf("session changed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(authPath); err != nil || string(data) != "existing credential" {
		t.Fatalf("credential changed: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(replacementHome, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("wrote state into ephemeral HOME: %v", err)
	}
	env := codexEnv(config.Config{}, state, replacementHome)
	if env["CODEX_HOME"] != state || env["HOME"] != replacementHome {
		t.Fatalf("unexpected runtime paths: %+v", env)
	}
}

func TestCodexMappingKeepsLegacyLocationAndSeparatesStateVolumes(t *testing.T) {
	home := t.TempDir()
	record := codexbridge.ThreadMappingRecord{InstanceID: "instance-1", CodexThreadID: "session-1", CreatedAtUnixMs: 1, LastUsedAtUnixMs: 1}
	if err := codexbridge.NewThreadMappingStore(home).Save(record); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := codexMappingStore(filepath.Join(home, ".codex"), home).Load(record.InstanceID); err != nil || !ok {
		t.Fatalf("legacy mapping lost: %v, %v", ok, err)
	}
	if _, ok, err := codexMappingStore(t.TempDir(), home).Load(record.InstanceID); err != nil || ok {
		t.Fatalf("different state volume inherited a mapping: %v, %v", ok, err)
	}
}
