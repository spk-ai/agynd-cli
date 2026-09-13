package daemon

import (
	"path/filepath"

	"github.com/agynio/agynd-cli/internal/codexbridge"
)

func codexMappingStore(codexHome, home string) *codexbridge.ThreadMappingStore {
	// Keep legacy mappings discoverable when no state relocation was requested.
	if filepath.Clean(codexHome) == filepath.Join(home, ".codex") {
		return codexbridge.NewThreadMappingStore(home)
	}
	return codexbridge.NewThreadMappingStoreAtDir(filepath.Join(codexHome, "agyn", "thread-mapping"))
}
