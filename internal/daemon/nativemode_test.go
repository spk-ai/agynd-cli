package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agynio/agynd-cli/internal/config"
)

func TestWriteClaudeSettingsNativeOmitsEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	servers := []config.MCPServer{{Name: "platform", Port: 9100}}
	if err := writeClaudeSettings("http://llm-proxy.agyn:443", "token", servers, true); err != nil {
		t.Fatalf("write claude settings: %v", err)
	}

	payload, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings claudeSettings
	if err := json.Unmarshal(payload, &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}

	if _, ok := settings.Env["ANTHROPIC_BASE_URL"]; ok {
		t.Fatal("native settings carry a base URL")
	}
	if _, ok := settings.Env["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("native settings carry a credential")
	}
	// The whole point of still writing the file.
	if settings.Permissions.DefaultMode != "bypassPermissions" {
		t.Fatalf("native settings lost permissions: %+v", settings.Permissions)
	}
	declared, _ := readState(t, home)["mcpServers"].(map[string]any)
	if _, ok := declared["platform"]; !ok {
		t.Fatalf("native mode lost MCP wiring: %+v", declared)
	}
	// Neither is endpoint configuration, and an autoupdate check in native mode
	// reaches a host nothing intercepts.
	if settings.Env["DISABLE_AUTOUPDATER"] != "1" {
		t.Fatal("native settings dropped the autoupdater guard")
	}
	if settings.Env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] != "1" {
		t.Fatal("native settings dropped the nonessential traffic guard")
	}
}

func TestWriteClaudeSettingsPlatformKeepsEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := writeClaudeSettings("http://llm-proxy.agyn:443", "token", nil, false); err != nil {
		t.Fatalf("write claude settings: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings claudeSettings
	if err := json.Unmarshal(payload, &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if settings.Env["ANTHROPIC_BASE_URL"] != "http://llm-proxy.agyn:443" {
		t.Fatalf("platform settings lost the base URL: %+v", settings.Env)
	}
	if settings.Env["ANTHROPIC_API_KEY"] != "token" {
		t.Fatalf("platform settings lost the credential: %+v", settings.Env)
	}
}

func TestCodexConfigNativeOmitsProvider(t *testing.T) {
	cfg := config.Config{
		LLMBaseURL: "http://llm-proxy.agyn:443/v1",
		LLMNative:  true,
		MCPServers: []config.MCPServer{{Name: "platform", Port: 9100}},
	}
	payload := codexConfig(cfg)

	// No endpoint and no credential. The provider below names neither -- it
	// only settles transport, and codex keeps addressing its own vendor.
	for _, forbidden := range []string{"base_url", "llm-proxy.agyn", "env_key"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("native codex config carries %q:\n%s", forbidden, payload)
		}
	}
	// The hook lives in the system config, not this one -- it is the only layer
	// codex trusts a hook from.
	for _, required := range []string{
		"[mcp_servers.platform]",
		"approval_policy",
		// Left on its default, codex opens a WebSocket the proxy cannot
		// terminate and the vendor answers 404.
		"supports_websockets = false",
		"requires_openai_auth = true",
		// Its Apps MCP wants a ChatGPT session cookie, which a subscription
		// token is not, so it fails on every start.
		"[mcp_servers.codex_apps]",
	} {
		if !strings.Contains(payload, required) {
			t.Fatalf("native codex config lost %q:\n%s", required, payload)
		}
	}
}

func TestCodexEnvNativeKeepsPlaceholder(t *testing.T) {
	cfg := config.Config{LLMBaseURL: "http://llm-proxy.agyn:443/v1", LLMNative: true, LLMAPIToken: "platform"}
	env := codexEnv(cfg, "/home/agent/.codex", "/home/agent")

	// The placeholder codex reads is on the container; overwriting the variable
	// here with a platform token would replace it with something the proxy does
	// not expect and the vendor would never accept.
	if _, ok := env[codexEnvOpenAIAPIKey]; ok {
		t.Fatalf("native codex env sets a platform credential: %+v", env)
	}
	if env[codexEnvNoProxy] == "" {
		t.Fatal("native codex env lost the ziti no_proxy list")
	}
}

func TestModelSelectionFollowsMode(t *testing.T) {
	const platformModel = "1c9b3a1e-0000-4000-8000-000000000000"
	cases := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"platform uses the model UUID", config.Config{}, platformModel},
		{"native uses the vendor model name", config.Config{LLMNative: true, LLMModelName: "claude-sonnet-4-6"}, "claude-sonnet-4-6"},
		{"native without a name leaves the CLI on its default", config.Config{LLMNative: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeModel(tc.cfg, platformModel); got != tc.want {
				t.Fatalf("claudeModel = %q, want %q", got, tc.want)
			}
			if got := codexModel(tc.cfg, platformModel); got != tc.want {
				t.Fatalf("codexModel = %q, want %q", got, tc.want)
			}
		})
	}
}
