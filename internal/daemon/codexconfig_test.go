package daemon

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agynio/agynd-cli/internal/config"
	"github.com/agynio/agynd-cli/internal/tracing"
	codex "github.com/agynio/codex-sdk-go"
)

type noopCodexClient struct{}

func (noopCodexClient) StartThread(context.Context, *codex.ThreadStartParams) (*codex.ThreadStartResponse, error) {
	return nil, nil
}

func (noopCodexClient) ResumeThread(context.Context, *codex.ThreadResumeParams) (*codex.ThreadResumeResponse, error) {
	return nil, nil
}

func (noopCodexClient) ReadThread(context.Context, *codex.ThreadReadParams) (*codex.ThreadReadResponse, error) {
	return nil, nil
}

func (noopCodexClient) StartTurn(context.Context, *codex.TurnStartParams) (*codex.TurnStartResponse, error) {
	return nil, nil
}

func (noopCodexClient) Close() error {
	return nil
}

func TestWriteCodexConfig(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	baseURL := "https://example.com"
	codexHome, err := writeCodexConfig(config.Config{LLMBaseURL: baseURL, MCPServers: nil})
	if err != nil {
		t.Fatalf("expected config to be written, got %v", err)
	}

	expectedHome := filepath.Join(tmpHome, ".codex")
	if codexHome != expectedHome {
		t.Fatalf("expected codex home %q, got %q", expectedHome, codexHome)
	}

	configPath := filepath.Join(codexHome, "config.toml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("expected config to be readable, got %v", err)
	}

	expected := codexConfigWithAPIKey(baseURL)
	if string(content) != expected {
		t.Fatalf("expected config %q, got %q", expected, string(content))
	}
}

func TestWriteCodexConfigHomeFallback(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", "")

	baseURL := "https://example.com"
	codexHome, err := writeCodexConfig(config.Config{LLMBaseURL: baseURL, MCPServers: nil})
	if err != nil {
		t.Fatalf("expected config to be written, got %v", err)
	}

	expectedHome := filepath.Join(codexDefaultHome, ".codex")
	if codexHome != expectedHome {
		t.Fatalf("expected codex home %q, got %q", expectedHome, codexHome)
	}

	configPath := filepath.Join(codexHome, "config.toml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("expected config to be readable, got %v", err)
	}

	expected := codexConfigWithAPIKey(baseURL)
	if string(content) != expected {
		t.Fatalf("expected config %q, got %q", expected, string(content))
	}
}

func TestWriteCodexConfigForZitiOmitsAPIKeyEnv(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	baseURL := "http://llm-proxy.agyn:443/v1"
	codexHome, err := writeCodexConfig(config.Config{LLMBaseURL: baseURL, MCPServers: nil})
	if err != nil {
		t.Fatalf("expected config to be written, got %v", err)
	}

	configPath := filepath.Join(codexHome, "config.toml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("expected config to be readable, got %v", err)
	}

	expected := codexConfigWithoutAPIKey(baseURL)
	if string(content) != expected {
		t.Fatalf("expected config %q, got %q", expected, string(content))
	}
}

func TestWriteCodexConfigWithMCPServers(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	baseURL := "https://example.com"
	mcpServers := []config.MCPServer{
		{Name: "memory", Port: 8100},
		{Name: "cache", Port: 8200},
	}
	codexHome, err := writeCodexConfig(config.Config{LLMBaseURL: baseURL, MCPServers: mcpServers})
	if err != nil {
		t.Fatalf("expected config to be written, got %v", err)
	}

	expectedHome := filepath.Join(tmpHome, ".codex")
	if codexHome != expectedHome {
		t.Fatalf("expected codex home %q, got %q", expectedHome, codexHome)
	}

	configPath := filepath.Join(codexHome, "config.toml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("expected config to be readable, got %v", err)
	}

	expected := codexConfigWithAPIKey(baseURL) +
		"\n[mcp_servers.memory]\n" +
		"url = \"http://127.0.0.1:8100/mcp\"\n" +
		"required = true\n" +
		"startup_timeout_sec = 120\n" +
		"\n[mcp_servers.cache]\n" +
		"url = \"http://127.0.0.1:8200/mcp\"\n" +
		"required = true\n" +
		"startup_timeout_sec = 120\n"
	if string(content) != expected {
		t.Fatalf("expected config %q, got %q", expected, string(content))
	}
}

func TestCodexEnvIncludesAPIKeyForPublicLLM(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	cfg := config.Config{
		LLMBaseURL:  "https://llm.example/v1",
		LLMAPIToken: "token-123",
	}

	env := codexEnv(cfg, "/tmp/.codex", "/tmp")

	if env[codexEnvOpenAIAPIKey] != "token-123" {
		t.Fatalf("expected API key in codex env, got %q", env[codexEnvOpenAIAPIKey])
	}
	assertCodexBaseEnv(t, env)
}

func TestCodexEnvOmitsAPIKeyForZitiLLM(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv(codexEnvNoProxy, "localhost, llm-proxy.agyn")
	t.Setenv(codexEnvNoProxyLower, "127.0.0.1")
	cfg := config.Config{
		LLMBaseURL:  "http://llm-proxy.agyn:443/v1",
		LLMAPIToken: "user-token",
	}

	env := codexEnv(cfg, "/tmp/.codex", "/tmp")

	if _, ok := env[codexEnvOpenAIAPIKey]; ok {
		t.Fatalf("expected ziti codex env to omit %s", codexEnvOpenAIAPIKey)
	}
	if env[codexEnvNoProxy] != "localhost,llm-proxy.agyn,127.0.0.1,.agyn,gateway.agyn,tracing.agyn" {
		t.Fatalf("unexpected %s: %q", codexEnvNoProxy, env[codexEnvNoProxy])
	}
	if env[codexEnvNoProxyLower] != env[codexEnvNoProxy] {
		t.Fatalf("unexpected %s: %q", codexEnvNoProxyLower, env[codexEnvNoProxyLower])
	}
	assertCodexBaseEnv(t, env)
}

func TestCodexEnvMergesNoProxySpellingsForZitiLLM(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv(codexEnvNoProxy, "localhost, .AGYN")
	t.Setenv(codexEnvNoProxyLower, "127.0.0.1, localhost, gateway.agyn")
	cfg := config.Config{LLMBaseURL: "http://llm-proxy.agyn:443/v1"}

	env := codexEnv(cfg, "/tmp/.codex", "/tmp")

	want := "localhost,.AGYN,127.0.0.1,gateway.agyn,llm-proxy.agyn,tracing.agyn"
	if env[codexEnvNoProxyLower] != env[codexEnvNoProxy] {
		t.Fatalf("expected proxy bypass env spellings to match: %s=%q %s=%q", codexEnvNoProxy, env[codexEnvNoProxy], codexEnvNoProxyLower, env[codexEnvNoProxyLower])
	}
	if env[codexEnvNoProxy] != want {
		t.Fatalf("expected %s %q, got %q", codexEnvNoProxy, want, env[codexEnvNoProxy])
	}
	if env[codexEnvNoProxyLower] != want {
		t.Fatalf("expected %s %q, got %q", codexEnvNoProxyLower, want, env[codexEnvNoProxyLower])
	}
}

func TestCodexEnvClearsProxyVarsForZitiLLM(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	for _, key := range codexProxyEnvVars {
		t.Setenv(key, "http://proxy.invalid:8080")
	}
	cfg := config.Config{LLMBaseURL: "http://llm-proxy.agyn:443/v1"}

	env := codexEnv(cfg, "/tmp/.codex", "/tmp")

	for _, key := range codexProxyEnvVars {
		value, ok := env[key]
		if !ok {
			t.Fatalf("expected %s override", key)
		}
		if value != "" {
			t.Fatalf("expected %s to be cleared, got %q", key, value)
		}
	}
}

func TestCodexEnvDoesNotSetNoProxyForPublicLLM(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	cfg := config.Config{
		LLMBaseURL:  "https://llm.example/v1",
		LLMAPIToken: "token-123",
	}

	env := codexEnv(cfg, "/tmp/.codex", "/tmp")

	if _, ok := env[codexEnvNoProxy]; ok {
		t.Fatalf("expected public LLM env to omit %s", codexEnvNoProxy)
	}
	if _, ok := env[codexEnvNoProxyLower]; ok {
		t.Fatalf("expected public LLM env to omit %s", codexEnvNoProxyLower)
	}
	for _, key := range codexProxyEnvVars {
		if _, ok := env[key]; ok {
			t.Fatalf("expected public LLM env to omit %s", key)
		}
	}
}

func TestZitiNoProxyValue(t *testing.T) {
	got := zitiNoProxyValue(" localhost, .AGYN,,gateway.agyn ", "127.0.0.1,localhost")
	want := "localhost,.AGYN,gateway.agyn,127.0.0.1,llm-proxy.agyn,tracing.agyn"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestWithoutCodexAuthEnvRemovesParentAuthVarsDuringStart(t *testing.T) {
	t.Setenv(codexEnvOpenAIAPIKey, "parent-openai-token")
	t.Setenv(codexEnvCodexAPIKey, "parent-codex-token")
	t.Setenv(codexEnvCodexAccessToken, "parent-access-token")

	seenEnv := map[string]bool{}
	_, err := withoutCodexAuthEnv(func() (codexClient, error) {
		for _, key := range codexAuthEnvVars {
			_, seenEnv[key] = os.LookupEnv(key)
		}
		return noopCodexClient{}, nil
	})
	if err != nil {
		t.Fatalf("expected auth env scrub to succeed, got %v", err)
	}

	for _, key := range codexAuthEnvVars {
		if seenEnv[key] {
			t.Fatalf("expected %s to be unset while starting ziti Codex", key)
		}
	}
	if got := os.Getenv(codexEnvOpenAIAPIKey); got != "parent-openai-token" {
		t.Fatalf("expected OPENAI_API_KEY restore, got %q", got)
	}
	if got := os.Getenv(codexEnvCodexAPIKey); got != "parent-codex-token" {
		t.Fatalf("expected CODEX_API_KEY restore, got %q", got)
	}
	if got := os.Getenv(codexEnvCodexAccessToken); got != "parent-access-token" {
		t.Fatalf("expected CODEX_ACCESS_TOKEN restore, got %q", got)
	}
}

func TestZitiCodexProcessReceivesNoAuthEnvConfig(t *testing.T) {
	t.Setenv(codexEnvOpenAIAPIKey, "parent-openai-token")
	t.Setenv(codexEnvCodexAPIKey, "parent-codex-token")
	t.Setenv(codexEnvCodexAccessToken, "parent-access-token")
	t.Setenv("PATH", "/usr/bin")
	cfg := config.Config{
		LLMBaseURL:  "http://llm-proxy.agyn:443/v1",
		LLMAPIToken: "agent-env-token",
	}

	env := codexEnv(cfg, "/tmp/.codex", "/tmp")
	configPayload := codexConfig(config.Config{LLMBaseURL: cfg.LLMBaseURL, MCPServers: nil})
	seenEnv := map[string]bool{}
	_, err := withoutCodexAuthEnv(func() (codexClient, error) {
		for _, key := range codexAuthEnvVars {
			_, seenEnv[key] = os.LookupEnv(key)
			if _, ok := env[key]; ok {
				t.Fatalf("expected ziti codex env overrides to omit %s", key)
			}
		}
		return noopCodexClient{}, nil
	})
	if err != nil {
		t.Fatalf("expected auth env scrub to succeed, got %v", err)
	}

	for _, key := range codexAuthEnvVars {
		if seenEnv[key] {
			t.Fatalf("expected parent %s to be absent during ziti Codex start", key)
		}
	}
	if strings.Contains(configPayload, "env_key") {
		t.Fatalf("expected ziti codex config to omit env_key, got %q", configPayload)
	}
}

func TestIsZitiLLMBaseURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "platform proxy", url: "http://llm-proxy.agyn:443/v1", want: true},
		{name: "nested overlay host", url: "https://models.mesh.agyn/v1", want: true},
		{name: "uppercase host", url: " HTTP://LLM-PROXY.AGYN:443/v1 ", want: true},
		{name: "public endpoint", url: "https://llm.example/v1", want: false},
		{name: "platform's own domain", url: "https://llm-proxy.agyn.dev/v1", want: false},
		{name: "overlay tld as registrable suffix", url: "https://exampleagyn/v1", want: false},
		{name: "invalid url", url: "://bad", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isZitiLLMBaseURL(tt.url); got != tt.want {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func assertCodexBaseEnv(t *testing.T, env map[string]string) {
	t.Helper()
	if env[codexEnvPath] != agentPathValue() {
		t.Fatalf("expected PATH %q, got %q", agentPathValue(), env[codexEnvPath])
	}
	if env[codexEnvCodexHome] != "/tmp/.codex" {
		t.Fatalf("expected CODEX_HOME, got %q", env[codexEnvCodexHome])
	}
	if env[codexEnvHome] != "/tmp" {
		t.Fatalf("expected HOME, got %q", env[codexEnvHome])
	}
}

// The hook writes into the trace agynd opened, so it has to be handed the same
// one agynd is exporting the invocation message into.
func TestCodexEnvCarriesTheTraceAgyndOpened(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	cfg := config.Config{LLMBaseURL: "https://llm.example/v1", WorkloadID: "workload-1"}

	env := codexEnv(cfg, "/tmp/.codex", "/tmp")

	want := hex.EncodeToString(tracing.TraceID("workload-1"))
	if env[traceHookTraceEnv] != want {
		t.Fatalf("expected %s %q, got %q", traceHookTraceEnv, want, env[traceHookTraceEnv])
	}
}

func codexConfigWithAPIKey(baseURL string) string {
	return codexConfigPayload(baseURL, `env_key = "OPENAI_API_KEY"
`)
}

func codexConfigWithoutAPIKey(baseURL string) string {
	return codexConfigPayload(baseURL, "")
}

func codexConfigPayload(baseURL, apiKeyEnv string) string {
	return fmt.Sprintf(`model_provider = "platform"
approval_policy = "never"
sandbox_mode = "danger-full-access"

[model_providers.platform]
name = "Agyn LLM"
base_url = %q
%swire_api = "responses"
request_max_retries = 2
stream_max_retries = 2
supports_websockets = false
mcp_oauth_credentials_store = "file"
`, baseURL, apiKeyEnv)
}

// Codex registers a hook only when it is managed or carries a matching trust
// hash. The system layer is the managed one, so a hook written anywhere else is
// discovered, found untrusted and dropped in silence.
func TestWriteCodexConfigRegistersTheHookWhereCodexTrustsIt(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	systemConfig := filepath.Join(t.TempDir(), "etc", "codex", "config.toml")
	original := codexSystemConfigPath
	codexSystemConfigPath = systemConfig
	t.Cleanup(func() { codexSystemConfigPath = original })

	if _, err := writeCodexConfig(config.Config{LLMBaseURL: "https://example.com"}); err != nil {
		t.Fatalf("expected config to be written, got %v", err)
	}

	content, err := os.ReadFile(systemConfig)
	if err != nil {
		t.Fatalf("expected the system config to be readable, got %v", err)
	}
	if string(content) != codexHookTemplate {
		t.Fatalf("expected the hook in the system config, got %q", string(content))
	}
	// The user config is not a layer codex trusts hooks from, so declaring it
	// there as well would be a second untrusted copy rather than a fallback.
	userConfig, err := os.ReadFile(filepath.Join(tmpHome, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("expected the user config to be readable, got %v", err)
	}
	if strings.Contains(string(userConfig), "hooks.Stop") {
		t.Fatal("expected no hook in the user config")
	}
}

// Tracing is optional: an agent whose hook cannot be registered still answers.
func TestWriteCodexConfigSurvivesAnUnwritableSystemConfig(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", t.TempDir())
	original := codexSystemConfigPath
	codexSystemConfigPath = filepath.Join(t.TempDir(), "file", "config.toml")
	if err := os.WriteFile(filepath.Dir(filepath.Dir(codexSystemConfigPath))+"/file", nil, 0o600); err != nil {
		t.Fatalf("seed an unwritable path: %v", err)
	}
	t.Cleanup(func() { codexSystemConfigPath = original })

	if _, err := writeCodexConfig(config.Config{LLMBaseURL: "https://example.com"}); err != nil {
		t.Fatalf("expected the config to be written anyway, got %v", err)
	}
}

// A workload has no keyring, and codex's default MCP credential store is the OS
// secret service. Without this the servers are configured, reachable, and
// register no tools at all -- the model is offered `shell` alone and any MCP
// call comes back "unsupported call".
func TestCodexConfigStoresMCPCredentialsInAFile(t *testing.T) {
	payload := codexConfig(config.Config{LLMBaseURL: "https://example.com"})
	if !strings.Contains(payload, `mcp_oauth_credentials_store = "file"`) {
		t.Fatalf("expected a file credential store in the platform config, got %q", payload)
	}

	native := codexConfig(config.Config{LLMBaseURL: "https://example.com", LLMNative: true})
	if !strings.Contains(native, `mcp_oauth_credentials_store = "file"`) {
		t.Fatalf("expected a file credential store in the native config, got %q", native)
	}
}
