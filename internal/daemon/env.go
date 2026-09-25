package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	agentBinPath     = "/agyn/bin"
	codexDefaultHome = "/tmp"
)

func prependCLIPath(pathValue string) string {
	if pathValue == "" {
		return agentBinPath
	}
	return agentBinPath + string(os.PathListSeparator) + pathValue
}

func agentPathValue() string {
	return prependCLIPath(os.Getenv("PATH"))
}

func codexHomeEnv() string {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		return codexDefaultHome
	}
	return home
}

// codexStateHome resolves configuration, auth, and native session storage without
// changing HOME or the work directory. A nonblank CODEX_HOME must be absolute;
// otherwise storage stays under HOME/.codex (or /tmp/.codex when HOME is blank).
func codexStateHome() (string, error) {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("CODEX_HOME must be an absolute path")
		}
		return filepath.Clean(home), nil
	}
	return filepath.Join(codexHomeEnv(), ".codex"), nil
}
