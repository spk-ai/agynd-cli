package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const claudeStateFileName = ".claude.json"

// What the CLI must already believe about itself to start into work rather than
// onboarding. Everything else in the file is its own. Theme and the bypass
// disclaimer are settings.json keys, not state -- see writeClaudeSettings.
var claudeStateKeys = map[string]any{
	"hasCompletedOnboarding": true,
	"installMethod":          "native",
}

// writeClaudeState merges the first-run keys into ~/.claude.json. The file is
// the CLI's own state and accumulates across runs, so absent keys are set and
// the rest is left as found -- unlike a placeholder credential, which is
// skipped outright once a file exists.
func writeClaudeState() error {
	path, err := claudeStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create Claude state directory: %w", err)
	}
	state, err := readClaudeState(path)
	if err != nil {
		return err
	}
	changed := false
	for key, value := range claudeStateKeys {
		if _, ok := state[key]; ok {
			continue
		}
		state[key] = value
		changed = true
	}
	if !changed {
		return nil
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claude state: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Unreadable state starts over rather than failing: the CLI rewrites this file
// itself, and refusing to start over it would strand every workload.
func readClaudeState(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	state := map[string]any{}
	if err := json.Unmarshal(data, &state); err != nil || state == nil {
		if _, enabled := os.LookupEnv("AGYN_CLAUDE_SESSION_DIR"); enabled {
			return nil, fmt.Errorf("invalid persistent Claude user state; reconciliation required")
		}
		log.Printf("%s is not a JSON object (%v); writing first-run state over it", path, err)
		return map[string]any{}, nil
	}
	return state, nil
}
