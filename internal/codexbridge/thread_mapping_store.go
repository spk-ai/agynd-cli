package codexbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	threadMappingDirMode  = 0o700
	threadMappingFileMode = 0o600
)

type ThreadMappingRecord struct {
	InstanceID       string `json:"instance_id"`
	CodexThreadID    string `json:"codex_thread_id"`
	CreatedAtUnixMs  int64  `json:"created_at_unix_ms"`
	LastUsedAtUnixMs int64  `json:"last_used_at_unix_ms"`
}

func (r ThreadMappingRecord) validate() error {
	if r.InstanceID == "" {
		return fmt.Errorf("instance_id is required")
	}
	if r.CodexThreadID == "" {
		return fmt.Errorf("codex_thread_id is required")
	}
	if r.CreatedAtUnixMs <= 0 {
		return fmt.Errorf("created_at_unix_ms is required")
	}
	if r.LastUsedAtUnixMs <= 0 {
		return fmt.Errorf("last_used_at_unix_ms is required")
	}
	if r.LastUsedAtUnixMs < r.CreatedAtUnixMs {
		return fmt.Errorf("last_used_at_unix_ms precedes created_at_unix_ms")
	}
	if strings.TrimSpace(r.InstanceID) != r.InstanceID {
		return fmt.Errorf("instance_id contains whitespace")
	}
	if strings.TrimSpace(r.CodexThreadID) != r.CodexThreadID {
		return fmt.Errorf("codex_thread_id contains whitespace")
	}
	return nil
}

type ThreadMappingStore struct {
	dir        string
	createTemp func(dir, pattern string) (*os.File, error)
	rename     func(oldpath, newpath string) error
}

func NewThreadMappingStore(homeDir string) *ThreadMappingStore {
	return NewThreadMappingStoreAtDir(filepath.Join(homeDir, ".agyn", "codex", "thread-mapping"))
}

func NewThreadMappingStoreAtDir(dir string) *ThreadMappingStore {
	return &ThreadMappingStore{
		dir:        dir,
		createTemp: os.CreateTemp,
		rename:     os.Rename,
	}
}

func (s *ThreadMappingStore) Load(instanceID string) (ThreadMappingRecord, bool, error) {
	path, err := s.mappingPath(instanceID)
	if err != nil {
		return ThreadMappingRecord{}, false, err
	}
	record, ok, err := s.readRecord(path, instanceID)
	if err != nil {
		return ThreadMappingRecord{}, false, err
	}
	if !ok {
		return ThreadMappingRecord{}, false, nil
	}
	return record, true, nil
}

func (s *ThreadMappingStore) Save(record ThreadMappingRecord) error {
	if err := record.validate(); err != nil {
		return fmt.Errorf("invalid mapping: %w", err)
	}
	path, err := s.mappingPath(record.InstanceID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, threadMappingDirMode); err != nil {
		return fmt.Errorf("create mapping dir: %w", err)
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal mapping: %w", err)
	}
	file, err := s.createTemp(s.dir, "tmp-*.json")
	if err != nil {
		return fmt.Errorf("create temp mapping: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	if _, err := file.Write(payload); err != nil {
		cleanup()
		return fmt.Errorf("write mapping: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync mapping: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close mapping: %w", err)
	}
	if err := s.rename(file.Name(), path); err != nil {
		cleanup()
		return fmt.Errorf("rename mapping: %w", err)
	}
	if err := os.Chmod(path, threadMappingFileMode); err != nil {
		return fmt.Errorf("chmod mapping: %w", err)
	}
	return nil
}

func (s *ThreadMappingStore) mappingPath(instanceID string) (string, error) {
	return mappingPath(s.dir, instanceID)
}

func (s *ThreadMappingStore) readRecord(path, instanceID string) (ThreadMappingRecord, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ThreadMappingRecord{}, false, nil
		}
		return ThreadMappingRecord{}, false, fmt.Errorf("read mapping: %w", err)
	}
	var record ThreadMappingRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return ThreadMappingRecord{}, false, fmt.Errorf("parse mapping: %w", err)
	}
	if err := record.validate(); err != nil {
		return ThreadMappingRecord{}, false, fmt.Errorf("invalid mapping: %w", err)
	}
	if record.InstanceID != instanceID {
		return ThreadMappingRecord{}, false, fmt.Errorf("mapping instance_id mismatch")
	}
	return record, true, nil
}

func mappingPath(dir, instanceID string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", fmt.Errorf("mapping directory is required")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("mapping directory %q must be absolute", dir)
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return "", fmt.Errorf("instance id is required")
	}
	if strings.ContainsRune(instanceID, filepath.Separator) || filepath.Base(instanceID) != instanceID {
		return "", fmt.Errorf("instance id %q is invalid", instanceID)
	}
	return filepath.Join(dir, instanceID+".json"), nil
}
