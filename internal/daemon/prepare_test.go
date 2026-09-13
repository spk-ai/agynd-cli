package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agynio/agynd-cli/internal/config"
)

// A sandbox spawns no agent CLI, but it is the workload a person starts one
// inside by hand -- so it is the only one where an unprepared CLI stops to ask
// a question.
func TestHolderModePreparesTheAgentCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.Config{
		Mode:       config.ModeHolder,
		SDK:        SDKClaude,
		WorkDir:    t.TempDir(),
		LLMBaseURL: "http://llm-proxy.agyn:443/v1",
		MCPServers: []config.MCPServer{{Name: "files", Port: 9100}},
	}
	if _, err := New(context.Background(), cfg, "test"); err != nil {
		t.Fatalf("new holder daemon: %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, claudeStateFileName)); err != nil {
		t.Fatalf("first-run state not written: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("settings not written: %v", err)
	}
	var settings claudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	if settings.Permissions.DefaultMode != "bypassPermissions" {
		t.Fatalf("defaultMode = %q", settings.Permissions.DefaultMode)
	}
	if _, ok := settings.MCPServers["files"]; !ok {
		t.Fatalf("mcp wiring lost: %v", settings.MCPServers)
	}
}

// An environment naming no agent runtime image carries no CLI to prepare.
func TestHolderModeWithoutAnAgentCLIPreparesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.Config{Mode: config.ModeHolder, SDK: config.ModeHolder, WorkDir: t.TempDir()}
	if _, err := New(context.Background(), cfg, "test"); err != nil {
		t.Fatalf("new holder daemon: %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, claudeStateFileName)); !os.IsNotExist(err) {
		t.Fatalf("wrote state for a workload with no CLI: %v", err)
	}
}

// The agent path writes settings.json only after the platform has resolved MCP
// ports, so preparation leaves it alone and writes the state file it can.
func TestAgentModePreparesStateButDefersSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.Config{Mode: config.ModeAgent, SDK: SDKClaude, LLMBaseURL: "http://llm-proxy.agyn:443/v1"}
	if err := prepareAgentCLI(cfg); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, claudeStateFileName)); err != nil {
		t.Fatalf("first-run state not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("wrote settings before MCP resolution: %v", err)
	}
}

// A native-mode codex gets its auth file from the CLI it is, not from anything
// the platform relays.
func TestPrepareWritesCodexAuthInNativeMode(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.Config{Mode: config.ModeHolder, SDK: SDKCodex, LLMNative: true, WorkDir: t.TempDir()}
	if err := prepareAgentCLI(cfg); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); err != nil {
		t.Fatalf("codex auth not written: %v", err)
	}
	// Without the config the CLI falls back to its own transport, which is a
	// WebSocket the proxy cannot terminate.
	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("codex config not written: %v", err)
	}
	if !strings.Contains(string(config), "supports_websockets = false") {
		t.Fatalf("codex config does not settle transport:\n%s", config)
	}
	if _, err := os.Stat(filepath.Join(home, claudeStateFileName)); !os.IsNotExist(err) {
		t.Fatalf("wrote Claude state for a Codex workload: %v", err)
	}
}

// Platform mode routes through the proxy by a platform model, and codex holds a
// real platform credential -- there is no subscription to stand in for.
func TestPrepareSkipsCodexAuthInPlatformMode(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.Config{Mode: config.ModeHolder, SDK: SDKCodex, WorkDir: t.TempDir()}
	if err := prepareAgentCLI(cfg); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("wrote auth.json in platform mode: %v", err)
	}
}
