package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agynio/agynd-cli/internal/claudebridge"
	"github.com/agynio/agynd-cli/internal/config"
)

var errClaudeSessionMismatch = errors.New("native Claude session identity mismatch; reconciliation required")

func claudeConfigDir() (string, error) {
	if directory, set := os.LookupEnv("CLAUDE_CONFIG_DIR"); set {
		if !filepath.IsAbs(directory) {
			return "", fmt.Errorf("CLAUDE_CONFIG_DIR must be absolute")
		}
		return filepath.Clean(directory), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

func claudeStatePath() (string, error) {
	if _, set := os.LookupEnv("CLAUDE_CONFIG_DIR"); set {
		directory, err := claudeConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(directory, claudeStateFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, claudeStateFileName), nil
}

func prepareClaudeSession(cfg config.Config) (*claudebridge.Session, error) {
	directory, set := os.LookupEnv("AGYN_CLAUDE_SESSION_DIR")
	if !set {
		return nil, nil
	}
	if cfg.Mode == config.ModeHolder {
		return nil, fmt.Errorf("persistent Claude sessions require managed agent mode")
	}
	stateDir, err := claudeConfigDir()
	if err != nil {
		return nil, err
	}
	return claudebridge.Open(directory, claudebridge.Binding{AgentID: cfg.AgentID.String(),
		InstanceID: cfg.AgentInstanceID.String(), WorkDir: cfg.WorkDir, StateDir: stateDir})
}
