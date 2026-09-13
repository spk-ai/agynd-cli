package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agynio/agynd-cli/internal/config"
)

type claudeSettings struct {
	Permissions claudePermissions `json:"permissions"`
	// Without this the CLI downgrades bypassPermissions to the default mode and
	// waits on its disclaimer, which no agent workload can answer.
	SkipDangerousModePermissionPrompt bool                       `json:"skipDangerousModePermissionPrompt"`
	Theme                             string                     `json:"theme"`
	Env                               map[string]string          `json:"env"`
	MCPServers                        map[string]claudeMCPServer `json:"mcpServers,omitempty"`
	Hooks                             map[string][]claudeMatcher `json:"hooks,omitempty"`
}

type claudeMatcher struct {
	Hooks []claudeHook `json:"hooks"`
}

type claudeHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// traceHooks runs the platform's trace hook when a turn completes and when the
// session ends. Claude Code hands it the transcript, which is the record of
// what the turn actually did; its telemetry reports only that a call happened.
//
// SessionEnd as well as Stop, because a session can end with a turn whose
// completion never fired.
func traceHooks() map[string][]claudeMatcher {
	hook := claudeMatcher{Hooks: []claudeHook{{Type: "command", Command: traceHookCommand}}}
	return map[string][]claudeMatcher{
		"Stop":       {hook},
		"SessionEnd": {hook},
	}
}

type claudePermissions struct {
	DefaultMode string   `json:"defaultMode"`
	Allow       []string `json:"allow"`
	Deny        []string `json:"deny"`
}

type claudeMCPServer struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// writeClaudeSettings writes ~/.claude/settings.json. In native mode the
// endpoint and credential keys are omitted -- the CLI addresses its vendor
// directly and the placeholder credential comes from the container spec -- but
// the file is still written, because permissions and mcpServers live in the
// same document and dropping them would restore interactive tool approval and
// take the environment's MCP wiring with it.
func writeClaudeSettings(llmBaseURL, apiKey string, mcpServers []config.MCPServer, native bool) error {
	claudeDir, err := claudeConfigDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		return fmt.Errorf("create claude config dir: %w", err)
	}
	settings := claudeSettings{
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
		// Neither of these is endpoint or credential configuration, and both
		// matter more in native mode than in platform mode: only the vendor's
		// API host is intercepted, so any other call the CLI makes on its own
		// reaches nothing and waits for a timeout.
		Env: map[string]string{
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			"DISABLE_AUTOUPDATER":                      "1",
		},
	}
	if !native {
		settings.Env["ANTHROPIC_BASE_URL"] = llmBaseURL
		settings.Env["ANTHROPIC_API_KEY"] = apiKey
	}
	if len(mcpServers) > 0 {
		settings.MCPServers = make(map[string]claudeMCPServer, len(mcpServers))
		for _, server := range mcpServers {
			settings.MCPServers[server.Name] = claudeMCPServer{
				Type: "http",
				URL:  mcpEndpoint(server.Port),
			}
		}
	}
	payload, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claude settings: %w", err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, payload, 0o600); err != nil {
		return fmt.Errorf("write claude settings: %w", err)
	}
	return nil
}

func claudeBaseURL(llmBaseURL string) string {
	trimmed := strings.TrimSpace(llmBaseURL)
	trimmed = strings.TrimSuffix(trimmed, "/")
	return strings.TrimSuffix(trimmed, "/v1")
}
