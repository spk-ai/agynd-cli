package daemon

import (
	"path/filepath"

	"github.com/agynio/agynd-cli/internal/codexbridge"
)

// codexMappingStore keeps HOME/.agyn/codex/thread-mapping when the resolved state
// home is HOME/.codex, even for an explicit override to that default. Relocated
// state uses CODEX_HOME/agyn/thread-mapping; this selects a path, not a migration.
func codexMappingStore(codexHome, home string) *codexbridge.ThreadMappingStore {
	if filepath.Clean(codexHome) == filepath.Join(home, ".codex") {
		return codexbridge.NewThreadMappingStore(home)
	}
	return codexbridge.NewThreadMappingStoreAtDir(filepath.Join(codexHome, "agyn", "thread-mapping"))
}
