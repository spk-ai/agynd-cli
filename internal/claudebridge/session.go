// Package claudebridge owns durable native Claude session selection.
package claudebridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

type Binding struct {
	AgentID    string `json:"agent_id"`
	InstanceID string `json:"instance_id"`
	WorkDir    string `json:"work_dir"`
	StateDir   string `json:"state_dir"`
}

type record struct {
	Version int `json:"version"`
	Binding
	SessionID string `json:"session_id"`
}

// Session holds an exclusive daemon lock until Close. This does not fence
// escaped subprocesses or a failed node; the workload coordinator must do that.
type Session struct {
	ID         string
	Transcript string
	lock       *os.File
	closeOnce  sync.Once
	closeErr   error
}

// Open allocates an identity before the first CLI invocation or selects its
// exact existing transcript. It never replaces missing or ambiguous state.
// directory and StateDir must be separate private per-instance durable paths.
func Open(directory string, expected Binding) (_ *Session, err error) {
	if !validPath(directory) || !validPath(expected.StateDir) || !validPath(expected.WorkDir) ||
		!validID(expected.InstanceID) || !validID(expected.AgentID) {
		return nil, fmt.Errorf("invalid Claude session binding")
	}
	if directory == expected.StateDir {
		return nil, fmt.Errorf("Claude session mapping must be separate from native state")
	}
	created := false
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		if err := mkdirDurable(filepath.Dir(directory)); err != nil {
			return nil, err
		}
		if err := os.Mkdir(directory, 0700); err == nil {
			created = true
			if err := syncDirectory(filepath.Dir(directory)); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if err := privateDirectory(directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "session.json")
	if !created {
		// An initializing creator owns this new directory. Do not race it for
		// the lock before it has persisted a record.
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("required Claude session mapping is missing; reconciliation required")
		} else if err != nil {
			return nil, err
		}
	}
	lock, err := os.OpenFile(filepath.Join(directory, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	session := &Session{lock: lock}
	defer func() {
		if err != nil {
			_ = session.Close()
		}
	}()
	if err := privateFile(lock, 0); err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("Claude session already owned by another daemon: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		if !created {
			return nil, fmt.Errorf("required Claude session mapping is missing; reconciliation required")
		}
		if _, err := os.Lstat(expected.StateDir); err == nil {
			if err := privateDirectory(expected.StateDir); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := rejectExistingHistory(expected.StateDir); err != nil {
			return nil, err
		}
		if err := mkdirDurable(expected.StateDir); err != nil {
			return nil, err
		}
		if err := privateDirectory(expected.StateDir); err != nil {
			return nil, err
		}
		id, err := uuid.NewRandom()
		if err != nil {
			return nil, err
		}
		session.ID = id.String()
		payload, err := json.Marshal(record{Version: 1, Binding: expected, SessionID: session.ID})
		if err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		if _, err := file.Write(payload); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		if err := syncDirectory(directory); err != nil {
			return nil, err
		}
		return session, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := privateFile(file, 16384); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16385))
	decoder.DisallowUnknownFields()
	var current record
	if err := decoder.Decode(&current); err != nil {
		return nil, fmt.Errorf("invalid Claude session mapping: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || current.Version != 1 || current.Binding != expected || !validID(current.SessionID) {
		return nil, fmt.Errorf("Claude session identity or configuration mismatch")
	}
	if err := privateDirectory(expected.StateDir); err != nil {
		return nil, err
	}
	transcript, err := findTranscript(expected, current.SessionID)
	if err != nil {
		return nil, err
	}
	session.ID, session.Transcript = current.SessionID, transcript
	return session, nil
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.lock.Close() })
	return s.closeErr
}

func findTranscript(binding Binding, id string) (string, error) {
	projects := filepath.Join(binding.StateDir, "projects")
	info, err := os.Lstat(projects)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("required native Claude session history is missing or unsafe")
	}
	matches, err := filepath.Glob(filepath.Join(projects, "*", id+".jsonl"))
	if err != nil || len(matches) != 1 {
		return "", fmt.Errorf("required native Claude session transcript is missing or ambiguous")
	}
	path := matches[0]
	info, err = os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("native Claude project path is unsafe")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", fmt.Errorf("native Claude transcript must be a nonempty regular file")
	}
	// Read bounded identity metadata only; the CLI itself parses/resumes history.
	decoder := json.NewDecoder(io.LimitReader(file, 8*1024*1024))
	for i := 0; i < 1024; i++ {
		var entry struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if err := decoder.Decode(&entry); err != nil {
			return "", fmt.Errorf("native Claude transcript identity cannot be verified")
		}
		if entry.SessionID != "" && entry.SessionID != id {
			return "", fmt.Errorf("native Claude transcript session identity mismatch")
		}
		if entry.Type == "user" || entry.Type == "assistant" {
			if entry.SessionID != id || entry.Cwd != binding.WorkDir {
				return "", fmt.Errorf("native Claude transcript workspace or identity mismatch")
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("native Claude transcript identity exceeds inspection limit")
}

func rejectExistingHistory(stateDir string) error {
	return filepath.WalkDir(filepath.Join(stateDir, "projects"), func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || strings.HasSuffix(entry.Name(), ".jsonl") {
			return fmt.Errorf("native Claude history exists without a session binding; explicit migration required")
		}
		return nil
	})
}

func validID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validPath(value string) bool { return filepath.IsAbs(value) && filepath.Clean(value) == value }

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("Claude session directories must be private and not symlinks")
	}
	return nil
}

func privateFile(file *os.File, limit int64) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > limit {
		return fmt.Errorf("Claude session metadata must be a bounded private regular file")
	}
	return nil
}

func mkdirDurable(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("Claude state ancestor is not a directory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := mkdirDurable(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
