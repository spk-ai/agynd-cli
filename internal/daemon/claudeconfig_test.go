package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agynio/agynd-cli/internal/config"
)

func TestWriteClaudeSettings(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	baseURL := "https://example.com"
	apiKey := "test-api-key"
	if err := writeClaudeSettings(baseURL, apiKey, nil, false); err != nil {
		t.Fatalf("expected settings to be written, got %v", err)
	}

	settingsPath := filepath.Join(tmpHome, ".claude", "settings.json")
	content, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("expected settings to be readable, got %v", err)
	}

	var got claudeSettings
	if err := json.Unmarshal(content, &got); err != nil {
		t.Fatalf("expected settings to parse, got %v", err)
	}

	expected := claudeSettings{
		Hooks: traceHooks(),
		Permissions: claudePermissions{
			DefaultMode: "bypassPermissions",
			Allow: []string{
				"Bash",
				"Read",
				"Write",
				"Edit",
				"MultiEdit",
				"WebFetch",
				"WebSearch",
				"Grep",
				"Glob",
				"LS",
				"Task",
				"TodoWrite",
				"NotebookEdit",
			},
			Deny: []string{},
		},
		SkipDangerousModePermissionPrompt: true,
		Theme:                             "dark",
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":                       baseURL,
			"ANTHROPIC_API_KEY":                        apiKey,
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			"DISABLE_AUTOUPDATER":                      "1",
		},
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("expected settings %#v, got %#v", expected, got)
	}
}

// Claude Code 2.1 reads user-scoped MCP servers from its state file and ignores
// mcpServers in settings.json, where a declaration registered no tools.
func TestWriteClaudeSettingsDeclaresMCPServersInUserState(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	mcpServers := []config.MCPServer{
		{Name: "memory", Port: 8100},
		{Name: "cache", Port: 8200},
	}
	if err := writeClaudeSettings("https://example.com", "test-api-key", mcpServers, false); err != nil {
		t.Fatalf("expected settings to be written, got %v", err)
	}

	settings, err := os.ReadFile(filepath.Join(tmpHome, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("expected settings to be readable, got %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(settings, &raw); err != nil {
		t.Fatalf("expected settings to parse, got %v", err)
	}
	if _, ok := raw["mcpServers"]; ok {
		t.Fatalf("settings.json still declares mcpServers: %s", settings)
	}

	want := map[string]any{
		"memory": map[string]any{"type": "http", "url": "http://127.0.0.1:8100/mcp"},
		"cache":  map[string]any{"type": "http", "url": "http://127.0.0.1:8200/mcp"},
	}
	if got := readState(t, tmpHome)["mcpServers"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("mcpServers = %#v, want %#v", got, want)
	}
}

// Servers an init script installed (execution reporting), entries a workspace
// entrypoint declared first in the same shape, and unrelated CLI state all
// survive; only the platform's own names are replaced.
func TestDeclareClaudeMCPServersMergesIntoExistingUserState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	existing := `{
  "numStartups": 3,
  "hasCompletedOnboarding": true,
  "mcpServers": {
    "execution_reporting": {"type": "stdio", "command": "/agyn/bin/node", "args": ["/run/agyn-execution/runtime.mjs", "mcp", "/run/agyn-execution"]},
    "qa_browser": {"type": "http", "url": "http://127.0.0.1:9100/mcp"},
    "files": {"type": "http", "url": "http://127.0.0.1:1/stale"}
  }
}`
	if err := os.WriteFile(filepath.Join(home, claudeStateFileName), []byte(existing), 0o600); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	servers := []config.MCPServer{{Name: "qa_browser", Port: 9100}, {Name: "files", Port: 9200}}
	if err := declareClaudeMCPServers(servers); err != nil {
		t.Fatalf("declare: %v", err)
	}

	state := readState(t, home)
	if state["numStartups"] != float64(3) || state["hasCompletedOnboarding"] != true {
		t.Fatalf("unrelated user state lost: %v", state)
	}
	want := map[string]any{
		"execution_reporting": map[string]any{"type": "stdio", "command": "/agyn/bin/node",
			"args": []any{"/run/agyn-execution/runtime.mjs", "mcp", "/run/agyn-execution"}},
		"qa_browser": map[string]any{"type": "http", "url": "http://127.0.0.1:9100/mcp"},
		"files":      map[string]any{"type": "http", "url": "http://127.0.0.1:9200/mcp"},
	}
	if got := state["mcpServers"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("mcpServers = %#v, want %#v", got, want)
	}
	info, err := os.Stat(filepath.Join(home, claudeStateFileName))
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v, want 0600", info.Mode().Perm())
	}
}

// With CLAUDE_CONFIG_DIR set the CLI reads its user state there, not in HOME.
func TestDeclareClaudeMCPServersFollowsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "claude")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)

	if err := declareClaudeMCPServers([]config.MCPServer{{Name: "memory", Port: 8100}}); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, claudeStateFileName)); !os.IsNotExist(err) {
		t.Fatalf("declared into HOME despite CLAUDE_CONFIG_DIR: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(configDir, claudeStateFileName))
	if err != nil {
		t.Fatalf("read relocated state: %v", err)
	}
	if !strings.Contains(string(data), `"url": "http://127.0.0.1:8100/mcp"`) {
		t.Fatalf("relocated state lacks the server: %s", data)
	}
}

func TestDeclareClaudeMCPServersWithoutServersLeavesStateAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := declareClaudeMCPServers(nil); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, claudeStateFileName)); !os.IsNotExist(err) {
		t.Fatalf("state written without servers: %v", err)
	}
}

func TestDeclareClaudeMCPServersRejectsNonObjectServers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	existing := `{"mcpServers":["not","an","object"]}`
	path := filepath.Join(home, claudeStateFileName)
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if err := declareClaudeMCPServers([]config.MCPServer{{Name: "memory", Port: 8100}}); err == nil {
		t.Fatal("expected a non-object mcpServers to be rejected")
	}
	if data, _ := os.ReadFile(path); string(data) != existing {
		t.Fatalf("rejected state was rewritten: %s", data)
	}
}

func TestClaudeBaseURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "strip-v1",
			input: "http://llm-proxy.agyn:443/v1",
			want:  "http://llm-proxy.agyn:443",
		},
		{
			name:  "strip-v1-trailing-slash",
			input: "http://llm-proxy.agyn:443/v1/",
			want:  "http://llm-proxy.agyn:443",
		},
		{
			name:  "no-strip",
			input: "http://llm-proxy.agyn:443/v1beta",
			want:  "http://llm-proxy.agyn:443/v1beta",
		},
		{
			name:  "already-base",
			input: "http://llm-proxy.agyn:443",
			want:  "http://llm-proxy.agyn:443",
		},
		{
			name:  "trim-space",
			input: " http://llm-proxy.agyn:443/v1 ",
			want:  "http://llm-proxy.agyn:443",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := claudeBaseURL(test.input)
			if got != test.want {
				t.Fatalf("expected %q, got %q", test.want, got)
			}
		})
	}
}

// The hook is how a turn is recorded at all: Claude Code's own telemetry
// reports that a call happened, and the transcript it hands the hook is what
// says what was in it.
func TestWriteClaudeSettingsRegistersTheTraceHook(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if err := writeClaudeSettings("https://example.com", "test-api-key", nil, false); err != nil {
		t.Fatalf("write claude settings: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(tmpHome, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings claudeSettings
	if err := json.Unmarshal(content, &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}

	// SessionEnd as well as Stop: a session can end with a turn whose
	// completion never fired.
	for _, event := range []string{"Stop", "SessionEnd"} {
		matchers, ok := settings.Hooks[event]
		if !ok || len(matchers) != 1 || len(matchers[0].Hooks) != 1 {
			t.Fatalf("expected one %s hook, got %#v", event, settings.Hooks[event])
		}
		hook := matchers[0].Hooks[0]
		if hook.Type != "command" || hook.Command != traceHookCommand {
			t.Fatalf("unexpected %s hook: %#v", event, hook)
		}
	}
}

// Without the disclaimer accepted the CLI downgrades bypassPermissions to the
// default mode, so the permissions block alone does not survive contact.
func TestWriteClaudeSettingsAcceptsTheBypassDisclaimer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := writeClaudeSettings("http://llm-proxy.agyn:443", "platform", nil, false); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings claudeSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	if !settings.SkipDangerousModePermissionPrompt {
		t.Fatal("bypass disclaimer not accepted")
	}
	if settings.Theme == "" {
		t.Fatal("theme not set")
	}
}
