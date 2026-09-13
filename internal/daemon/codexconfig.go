package daemon

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agynio/agynd-cli/internal/config"
	"github.com/agynio/agynd-cli/internal/tracing"
	codex "github.com/agynio/codex-sdk-go"
)

// codexNativeConfigTemplate names no endpoint and no credential: codex still
// addresses its own vendor, and requires_openai_auth keeps it on the
// subscription credential it already reads from auth.json.
//
// What the provider does declare is transport. codex prefers a WebSocket to
// wss://chatgpt.com/backend-api/codex/responses, and the platform intercepts
// that connection to terminate it on the proxy -- which speaks HTTP, so the
// upgrade never completes and the vendor answers 404. supports_websockets
// settles it on HTTP/SSE, over the same host and path.
const codexNativeConfigTemplate = `model_provider = "openai_http"
approval_policy = "never"
sandbox_mode = "danger-full-access"

[model_providers.openai_http]
name = "OpenAI HTTP/SSE"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false

# codex's own Apps MCP authenticates with a ChatGPT session cookie, which a
# subscription token is not and the platform cannot mint -- it answers
# no_biscuit_no_service and warns on every start. Declaring the built-in here
# disables it. The command satisfies transport validation and never runs.
[mcp_servers.codex_apps]
enabled = false
command = "true"
`

// traceHookCommand is the platform's trace hook, delivered to /agyn/bin beside
// agynd and reached by name because that directory is on the agent's PATH.
const (
	traceHookCommand     = "agynd-trace-hook"
	traceHookFormatEnv   = "AGYN_TRACE_FORMAT"
	traceHookAddressEnv  = "TRACING_ADDRESS"
	traceHookTraceEnv    = "AGYN_TRACE_ID"
	traceparentEnv       = "TRACEPARENT"
	traceHookWorkloadEnv = "WORKLOAD_ID"

	traceFormatCodex  = "codex"
	traceFormatClaude = "claude"
)

// codexHookTemplate runs the platform's trace hook when a turn completes.
// Codex hands it the rollout path, which is the record of what the turn
// actually did -- its telemetry reports only that a call happened.
//
// It is written to the system config rather than the user one. Codex registers
// a hook only when it is managed or carries a trust hash matching its contents,
// and a hook declared in the user config is neither: it is discovered, found
// untrusted, and dropped without a word. Only the system layer is managed, and
// the flag that would bypass the check is a CLI argument the app-server
// subcommand does not accept.
const codexHookTemplate = `[[hooks.Stop]]
[[hooks.Stop.hooks]]
type = "command"
command = "` + traceHookCommand + `"
`

// The system config layer, which is what makes a hook declared in it managed.
// A variable so a test can write somewhere it is allowed to.
var codexSystemConfigPath = "/etc/codex/config.toml"

// A workload has no keyring. codex stores MCP OAuth tokens in the OS secret
// service by default, and in a container there is no dbus session to reach --
// it warns, and then registers no MCP tools at all:
//
//	mcp.runtime.refresh: failed to read OAuth tokens from keyring:
//	  Platform secure storage failure: no secret service provider or dbus session found
//	thread_start.dynamic_tool_count=0
//
// The model is then offered `shell` alone, so a call to any MCP tool comes back
// "unsupported call" -- a failure that names the tool rather than the store, one
// layer away from the cause. On a file store the same servers register: the
// model is offered mcp__filesystem and mcp__memory as it should be.
//
// File rather than none: the servers a workload runs are local and hold no
// OAuth today, but a server that does needs its tokens to survive the CLI
// restarting inside the same workload.
const codexCredentialsStore = `mcp_oauth_credentials_store = "file"
`

// A turn reaches the platform over the overlay, and a connection that served
// one turn may be gone by the next: the tunnel closes its circuit and the next
// request goes into it before anything notices. Refusing to retry made that
// fatal -- the tool call would run and the turn that should have reported it
// died on the way out, with nothing posted to the thread. Two is small enough
// that a provider genuinely refusing is still reported rather than hammered.
const codexConfigTemplate = `model_provider = "platform"
approval_policy = "never"
sandbox_mode = "danger-full-access"

[model_providers.platform]
name = "Agyn LLM"
base_url = %q
%swire_api = "responses"
request_max_retries = 2
stream_max_retries = 2
supports_websockets = false
`

const codexAPIKeyEnv = `env_key = "OPENAI_API_KEY"
`

const zitiHostnameSuffix = ".agyn"

const (
	codexEnvPath             = "PATH"
	codexEnvHome             = "HOME"
	codexEnvCodexHome        = "CODEX_HOME"
	codexEnvOpenAIAPIKey     = "OPENAI_API_KEY"
	codexEnvCodexAPIKey      = "CODEX_API_KEY"
	codexEnvCodexAccessToken = "CODEX_ACCESS_TOKEN"
	codexEnvNoProxy          = "NO_PROXY"
	codexEnvNoProxyLower     = "no_proxy"
	codexEnvHTTPProxy        = "HTTP_PROXY"
	codexEnvHTTPProxyLower   = "http_proxy"
	codexEnvHTTPSProxy       = "HTTPS_PROXY"
	codexEnvHTTPSProxyLower  = "https_proxy"
	codexEnvAllProxy         = "ALL_PROXY"
	codexEnvAllProxyLower    = "all_proxy"
)

var codexZitiNoProxyHosts = []string{
	".agyn",
	"llm-proxy.agyn",
	"gateway.agyn",
	"tracing.agyn",
}

var codexAuthEnvMu sync.Mutex

var codexAuthEnvVars = []string{
	codexEnvOpenAIAPIKey,
	codexEnvCodexAPIKey,
	codexEnvCodexAccessToken,
}

var codexProxyEnvVars = []string{
	codexEnvHTTPProxy,
	codexEnvHTTPProxyLower,
	codexEnvHTTPSProxy,
	codexEnvHTTPSProxyLower,
	codexEnvAllProxy,
	codexEnvAllProxyLower,
}

func writeCodexConfig(cfg config.Config) (string, error) {
	codexHome, err := codexStateHome()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return "", fmt.Errorf("create codex home dir: %w", err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	payload := codexConfig(cfg)
	if err := os.WriteFile(configPath, []byte(payload), 0o600); err != nil {
		return "", fmt.Errorf("write codex config: %w", err)
	}
	if err := writeCodexSystemConfig(); err != nil {
		// Tracing is optional: an agent that cannot be traced still answers.
		log.Printf("tracing: register trace hook: %v", err)
	}
	return codexHome, nil
}

// writeCodexSystemConfig declares the trace hook where codex will trust it.
func writeCodexSystemConfig() error {
	if err := os.MkdirAll(filepath.Dir(codexSystemConfigPath), 0o755); err != nil {
		return fmt.Errorf("create codex system config dir: %w", err)
	}
	if err := os.WriteFile(codexSystemConfigPath, []byte(codexHookTemplate), 0o644); err != nil {
		return fmt.Errorf("write codex system config: %w", err)
	}
	return nil
}

func codexPlatformConfig(llmBaseURL string) string {
	apiKeyEnv := codexAPIKeyEnv
	if isZitiLLMBaseURL(llmBaseURL) {
		apiKeyEnv = ""
	}
	return fmt.Sprintf(codexConfigTemplate, llmBaseURL, apiKeyEnv)
}

func codexConfig(cfg config.Config) string {
	payload := codexPlatformConfig(cfg.LLMBaseURL)
	if cfg.LLMNative {
		payload = codexNativeConfigTemplate
	}
	payload += codexCredentialsStore
	mcpServers := cfg.MCPServers
	if len(mcpServers) == 0 {
		return payload
	}
	var builder strings.Builder
	builder.WriteString(payload)
	for _, server := range mcpServers {
		url := mcpEndpoint(server.Port)
		fmt.Fprintf(&builder, "\n[mcp_servers.%s]\nurl = %q\nrequired = true\nstartup_timeout_sec = 120\n", server.Name, url)
	}
	return builder.String()
}

func codexEnv(cfg config.Config, codexHome, codexHomeValue string) map[string]string {
	env := map[string]string{
		codexEnvPath:      agentPathValue(),
		codexEnvCodexHome: codexHome,
		codexEnvHome:      codexHomeValue,
		// The hook is told which transcript it is being handed rather than
		// sniffing the file, where to export what it reads, and which trace to
		// write into -- the one agynd opened for this wake cycle.
		traceHookFormatEnv:   traceFormatCodex,
		traceHookAddressEnv:  cfg.TracingAddress,
		traceHookTraceEnv:    traceHookTraceID(cfg.WorkloadID),
		traceHookWorkloadEnv: cfg.WorkloadID,
	}
	// Native mode carries no platform credential at all: the placeholder codex
	// reads is on the container, and the proxy replaces it upstream.
	if cfg.LLMNative || isZitiLLMBaseURL(cfg.LLMBaseURL) {
		noProxyValue := zitiNoProxyValue(
			os.Getenv(codexEnvNoProxy),
			os.Getenv(codexEnvNoProxyLower),
		)
		env[codexEnvNoProxy] = noProxyValue
		env[codexEnvNoProxyLower] = noProxyValue
		for _, key := range codexProxyEnvVars {
			env[key] = ""
		}
	} else {
		env[codexEnvOpenAIAPIKey] = cfg.LLMAPIToken
	}
	return env
}

// The hook is handed the trace as hex rather than the workload it came from,
// so agynd stays the one that decides what a trace is.
func traceHookTraceID(workloadID string) string {
	return hex.EncodeToString(tracing.TraceID(workloadID))
}

// traceparentFor is the same trace in the W3C spelling, for a CLI that reports
// its own spans and reads a parent the way any OTel process does.
//
// The span id is the trace's first eight bytes rather than one drawn per call.
// Nothing here opens a span for agn to hang off, and a parent that changed
// between two starts of the same workload would leave the second cycle's turns
// pointing at something that never existed -- the same reason the trace id is
// derived from WORKLOAD_ID rather than drawn.
func traceparentFor(workloadID string) string {
	traceID := tracing.TraceID(workloadID)
	return "00-" + hex.EncodeToString(traceID) + "-" + hex.EncodeToString(traceID[:8]) + "-01"
}

func zitiNoProxyValue(values ...string) string {
	seen := make(map[string]struct{}, len(codexZitiNoProxyHosts))
	merged := make([]string, 0, len(codexZitiNoProxyHosts))
	for _, value := range values {
		for _, entry := range splitNoProxy(value) {
			key := strings.ToLower(entry)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, entry)
		}
	}
	for _, host := range codexZitiNoProxyHosts {
		key := strings.ToLower(host)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, host)
	}
	return strings.Join(merged, ",")
}

func splitNoProxy(value string) []string {
	parts := strings.Split(value, ",")
	entries := make([]string, 0, len(parts))
	for _, part := range parts {
		entry := strings.TrimSpace(part)
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

func newCodexClient(ctx context.Context, cfg config.Config, options ...codex.Option) (codexClient, error) {
	// Native mode is the one case where the ambient auth variables are the
	// point: the placeholder lives in them, and codex refuses to start without
	// one. Only the platform path strips them.
	if cfg.LLMNative || !isZitiLLMBaseURL(cfg.LLMBaseURL) {
		return codex.NewClient(ctx, options...)
	}
	return withoutCodexAuthEnv(func() (codexClient, error) {
		return codex.NewClient(ctx, options...)
	})
}

func withoutCodexAuthEnv(start func() (codexClient, error)) (codexClient, error) {
	codexAuthEnvMu.Lock()
	defer codexAuthEnvMu.Unlock()

	originalEnv := captureEnv(codexAuthEnvVars)
	for _, key := range codexAuthEnvVars {
		if err := os.Unsetenv(key); err != nil {
			restoreEnv(originalEnv)
			return nil, fmt.Errorf("unset %s: %w", key, err)
		}
	}

	client, err := start()
	restoreEnv(originalEnv)
	return client, err
}

type envValue struct {
	value string
	set   bool
}

func captureEnv(keys []string) map[string]envValue {
	values := make(map[string]envValue, len(keys))
	for _, key := range keys {
		value, ok := os.LookupEnv(key)
		values[key] = envValue{value: value, set: ok}
	}
	return values
}

func restoreEnv(values map[string]envValue) {
	for key, value := range values {
		if value.set {
			_ = os.Setenv(key, value.value)
			continue
		}
		_ = os.Unsetenv(key)
	}
}

func isZitiLLMBaseURL(llmBaseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(llmBaseURL))
	if err != nil {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	return strings.HasSuffix(hostname, zitiHostnameSuffix)
}
