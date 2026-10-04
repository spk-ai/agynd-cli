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

// writeClaudeSettings writes ~/.claude/settings.json and declares the
// environment's MCP servers in the CLI's user state (declareClaudeMCPServers).
// In native mode the endpoint and credential keys are omitted -- the CLI
// addresses its vendor directly and the placeholder credential comes from the
// container spec -- but the file is still written, because dropping it would
// restore interactive tool approval.
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
	payload, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claude settings: %w", err)
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	if err := os.WriteFile(settingsPath, payload, 0o600); err != nil {
		return fmt.Errorf("write claude settings: %w", err)
	}
	return declareClaudeMCPServers(mcpServers)
}

// declareClaudeMCPServers merges the environment's MCP servers into the
// mcpServers object of the CLI's user state (claudeStatePath), the user scope
// Claude Code reads MCP servers from. Claude Code 2.1 ignores mcpServers in
// settings.json, so a declaration there registered no tools.
//
// The platform owns only the names it configures: an entry of the same name is
// replaced with the loopback endpoint, and every other entry is kept -- servers
// an init script installs (execution reporting), servers a workspace entrypoint
// declared first in the same shape, and the user's own. Without servers the
// file is left untouched. A non-object mcpServers is an error rather than
// something to overwrite.
//
// It runs before init scripts and the CLI, which reads the file only at start.
func declareClaudeMCPServers(servers []config.MCPServer) error {
	if len(servers) == 0 {
		return nil
	}
	path, err := claudeStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Claude state directory: %w", err)
	}
	state, err := readClaudeState(path)
	if err != nil {
		return err
	}
	declared := map[string]any{}
	if existing, ok := state["mcpServers"]; ok && existing != nil {
		object, isObject := existing.(map[string]any)
		if !isObject {
			return fmt.Errorf("mcpServers in %s is not a JSON object", path)
		}
		declared = object
	}
	for _, server := range servers {
		declared[server.Name] = claudeMCPServer{Type: "http", URL: mcpEndpoint(server.Port)}
	}
	state["mcpServers"] = declared
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claude state: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func claudeBaseURL(llmBaseURL string) string {
	trimmed := strings.TrimSpace(llmBaseURL)
	trimmed = strings.TrimSuffix(trimmed, "/")
	return strings.TrimSuffix(trimmed, "/v1")
}
