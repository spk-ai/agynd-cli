// Package inboxjournal prevents automatic re-execution of ambiguous inbox turns.
// It is a crash-recovery guard, not an exactly-once guarantee for external tools.
package inboxjournal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/agynio/agynd-cli/internal/platform"
	"github.com/google/uuid"
)

type Control struct {
	Version           int      `json:"version"`
	InstanceID        string   `json:"instance_id"`
	AllowedMessageID  string   `json:"allowed_message_id"`
	AckOnlyMessageIDs []string `json:"ack_only_message_ids"`
}

type record struct {
	Version     int    `json:"version"`
	InstanceID  string `json:"instance_id"`
	MessageID   string `json:"message_id"`
	InboxItemID string `json:"inbox_item_id"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
}

type Journal struct {
	directory  string
	instanceID string
	control    *Control
}

// Open requires a private, durable, daemon-owned directory. Optional control is
// supplied by a trusted coordinator before the daemon begins consuming inboxes.
func Open(directory, instanceID, controlPath string) (*Journal, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, fmt.Errorf("inbox journal directory must be absolute and clean")
	}
	if id, err := uuid.Parse(instanceID); err != nil || id == uuid.Nil || id.String() != instanceID {
		return nil, fmt.Errorf("inbox journal requires an instance identity")
	}
	j := &Journal{directory: filepath.Join(directory, instanceID), instanceID: instanceID}
	if controlPath != "" {
		if !filepath.IsAbs(controlPath) {
			return nil, fmt.Errorf("inbox control path must be absolute")
		}
		var control Control
		if err := readJSON(controlPath, 65536, &control); err != nil {
			return nil, fmt.Errorf("read inbox control: %w", err)
		}
		if control.Version != 1 || control.InstanceID != instanceID || !validID(control.AllowedMessageID) || len(control.AckOnlyMessageIDs) > 256 {
			return nil, fmt.Errorf("invalid inbox control binding")
		}
		seen := map[string]bool{control.AllowedMessageID: true}
		for _, id := range control.AckOnlyMessageIDs {
			if !validID(id) || seen[id] {
				return nil, fmt.Errorf("invalid or overlapping inbox control messages")
			}
			seen[id] = true
		}
		j.control = &control
	}
	for _, path := range []string{directory, j.directory} {
		if err := mkdirDurable(path); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("inbox journal directory must be private and not a symlink")
		}
	}
	return j, nil
}

// Begin returns true only after persisting intent before any agent invocation.
// False means acknowledge only; an ambiguous pending record is always an error
// unless a trusted control file explicitly retires that exact message.
func (j *Journal) Begin(message platform.Message) (bool, error) {
	expected, err := j.expected(message)
	if err != nil {
		return false, err
	}
	ackOnly := false
	if j.control != nil {
		for _, id := range j.control.AckOnlyMessageIDs {
			ackOnly = ackOnly || id == message.ID
		}
		if message.ID != j.control.AllowedMessageID && !ackOnly {
			return false, fmt.Errorf("inbox message is not authorized for this workload")
		}
	}
	current, err := j.read(expected)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err == nil {
		if current.State != "pending" {
			return false, nil
		}
		if !ackOnly {
			return false, fmt.Errorf("inbox message %s has an ambiguous prior attempt; reconciliation required", message.ID)
		}
		expected.State = "ack_only"
		return false, j.replace(expected)
	}
	expected.State = "pending"
	if ackOnly {
		expected.State = "ack_only"
	}
	data, err := json.Marshal(expected)
	if err != nil {
		return false, err
	}
	// Exclusive creation makes two daemon processes unable to both begin a turn.
	file, err := os.OpenFile(j.path(expected.MessageID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	if err := writeAndSync(file, data); err != nil {
		return false, err
	}
	if err := syncDirectory(j.directory); err != nil {
		return false, err
	}
	return !ackOnly, nil
}

// Complete persists completion before the remote inbox ACK. A lost ACK can then
// be retried without running the model, publishing the final reply or tools.
func (j *Journal) Complete(message platform.Message) error {
	expected, err := j.expected(message)
	if err != nil {
		return err
	}
	current, err := j.read(expected)
	if err != nil {
		return err
	}
	if current.State != "pending" {
		return nil
	}
	expected.State = "completed"
	return j.replace(expected)
}

func (j *Journal) expected(message platform.Message) (record, error) {
	if !validID(message.ID) || !validID(message.InboxItemID) {
		return record{}, fmt.Errorf("journaled messages require message and inbox item identities")
	}
	// No prompt or credential is stored. Stable immutable fields bind an item to
	// its original content; mutable display handles deliberately do not participate.
	data, err := json.Marshal(struct {
		ID, InboxItemID, ThreadID, SenderID, Body string
		FileIDs                                   []string
	}{message.ID, message.InboxItemID, message.ThreadID, message.SenderID, message.Body, message.FileIDs})
	if err != nil {
		return record{}, err
	}
	hash := sha256.Sum256(data)
	return record{Version: 1, InstanceID: j.instanceID, MessageID: message.ID,
		InboxItemID: message.InboxItemID, Fingerprint: hex.EncodeToString(hash[:])}, nil
}

func (j *Journal) read(expected record) (record, error) {
	var current record
	if err := readJSON(j.path(expected.MessageID), 4096, &current); err != nil {
		return record{}, err
	}
	state := current.State
	current.State = ""
	if current != expected || (state != "pending" && state != "completed" && state != "ack_only") {
		return record{}, fmt.Errorf("inbox journal identity, content or state mismatch")
	}
	current.State = state
	return current, nil
}

func (j *Journal) path(messageID string) string {
	hash := sha256.Sum256([]byte(messageID))
	return filepath.Join(j.directory, hex.EncodeToString(hash[:])+".json")
}

func (j *Journal) replace(value record) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(j.directory, ".journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := writeAndSync(file, data); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), j.path(value.MessageID)); err != nil {
		return err
	}
	return syncDirectory(j.directory)
}

func validID(value string) bool { return value != "" && len(value) <= 256 }

func readJSON(path string, limit int64, result any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("journal and control files must be bounded private regular files")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, limit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("unexpected trailing journal content")
	}
	return nil
}

func writeAndSync(file *os.File, data []byte) error {
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func mkdirDurable(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("journal ancestor is not a directory")
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
