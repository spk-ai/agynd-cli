package claudebridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

func fixture(root string) (string, Binding) {
	return filepath.Join(root, "mapping"), Binding{
		AgentID: "68e1f8d3-0a94-48d6-a28b-c67dd3458058", InstanceID: "6ca86e74-bc7f-4298-9b36-8cfa872937d8",
		WorkDir: filepath.Join(root, "workspace"), StateDir: filepath.Join(root, "state"),
	}
}

func history(binding Binding, id string) (string, error) {
	directory := filepath.Join(binding.StateDir, "projects", "project")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	data, err := json.Marshal(map[string]string{"type": "user", "sessionId": id, "cwd": binding.WorkDir})
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, id+".jsonl")
	return path, os.WriteFile(path, data, 0600)
}

func TestSessionCreateResumeAndOwnership(t *testing.T) {
	directory, binding := fixture(t.TempDir())
	first, err := Open(directory, binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if !validID(first.ID) || first.ID == binding.InstanceID || first.Transcript != "" {
		t.Fatalf("unexpected new selection: %+v", first)
	}
	recordPath := filepath.Join(directory, "session.json")
	before, err := os.ReadFile(recordPath)
	if err != nil || !strings.Contains(string(before), first.ID) {
		t.Fatalf("identity was not persisted before returning: %v", err)
	}
	info, err := os.Stat(recordPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("mapping is not private")
	}
	path, err := history(binding, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(directory, binding); err == nil {
		_ = other.Close()
		t.Fatal("two daemons acquired the same session")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(directory, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.ID != first.ID || second.Transcript != path {
		t.Fatalf("resume selection = %+v", second)
	}
	after, err := os.ReadFile(recordPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("resumption rewrote the immutable mapping")
	}
}

func TestSessionRejectsMissingCorruptOrForeignState(t *testing.T) {
	for _, scenario := range []string{"missing-mapping", "lost-mapping-directory", "invalid-json", "null", "unknown-field", "trailing-json",
		"agent", "instance", "workspace", "state-path", "missing-native", "empty-native", "foreign-native", "wrong-native-workspace",
		"ambiguous-native", "symlink-mapping", "symlink-native", "fifo-mapping", "fifo-native", "public-mapping", "public-directory"} {
		t.Run(scenario, func(t *testing.T) {
			directory, binding := fixture(t.TempDir())
			session, err := Open(directory, binding)
			if err != nil {
				t.Fatal(err)
			}
			path, err := history(binding, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			mapping := filepath.Join(directory, "session.json")
			original, err := os.ReadFile(mapping)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing-mapping":
				err = os.Remove(mapping)
			case "lost-mapping-directory":
				err = os.RemoveAll(directory)
			case "invalid-json":
				err = os.WriteFile(mapping, []byte("{"), 0600)
			case "null":
				err = os.WriteFile(mapping, []byte("null"), 0600)
			case "unknown-field":
				err = os.WriteFile(mapping, append(original[:len(original)-1], []byte(`,"extra":true}`)...), 0600)
			case "trailing-json":
				err = os.WriteFile(mapping, append(original, []byte(" {}")...), 0600)
			case "agent":
				binding.AgentID = uuid.NewString()
			case "instance":
				binding.InstanceID = uuid.NewString()
			case "workspace":
				binding.WorkDir += "-other"
			case "state-path":
				binding.StateDir += "-other"
			case "missing-native":
				err = os.RemoveAll(binding.StateDir)
			case "empty-native":
				err = os.WriteFile(path, nil, 0600)
			case "foreign-native", "wrong-native-workspace":
				id, cwd := session.ID, binding.WorkDir
				if scenario == "foreign-native" {
					id = uuid.NewString()
				} else {
					cwd += "-other"
				}
				payload, _ := json.Marshal(map[string]string{"type": "user", "sessionId": id, "cwd": cwd})
				err = os.WriteFile(path, payload, 0600)
			case "ambiguous-native":
				other := filepath.Join(binding.StateDir, "projects", "other")
				err = os.Mkdir(other, 0700)
				if err == nil {
					err = os.WriteFile(filepath.Join(other, session.ID+".jsonl"), []byte("{}"), 0600)
				}
			case "symlink-mapping", "symlink-native", "fifo-mapping", "fifo-native":
				target := mapping
				if strings.HasSuffix(scenario, "native") {
					target = path
				}
				err = os.Rename(target, target+".original")
				if err == nil && strings.HasPrefix(scenario, "symlink") {
					err = os.Symlink(target+".original", target)
				} else if err == nil {
					err = syscall.Mkfifo(target, 0600)
				}
			case "public-mapping":
				err = os.Chmod(mapping, 0644)
			case "public-directory":
				err = os.Chmod(directory, 0755)
			}
			if err != nil {
				t.Fatal(err)
			}
			if reopened, err := Open(directory, binding); err == nil {
				_ = reopened.Close()
				t.Fatal("unsafe state authorized a new/resumed session")
			}
		})
	}
}

func TestSessionReservationWithoutHistoryIsNotFreshAgain(t *testing.T) {
	directory, binding := fixture(t.TempDir())
	first, err := Open(directory, binding)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	if next, err := Open(directory, binding); err == nil {
		_ = next.Close()
		t.Fatal("an allocated session without native history must require reconciliation")
	}
}

func TestSessionConcurrentInitializersAndIndependentInstances(t *testing.T) {
	directory, binding := fixture(t.TempDir())
	var group sync.WaitGroup
	winners := make(chan *Session, 16)
	for i := 0; i < cap(winners); i++ {
		group.Go(func() {
			if session, err := Open(directory, binding); err == nil {
				winners <- session
			}
		})
	}
	group.Wait()
	close(winners)
	var selected []*Session
	for session := range winners {
		selected = append(selected, session)
		defer session.Close()
	}
	if len(selected) != 1 {
		t.Fatalf("initializers acquired %d sessions, want one", len(selected))
	}
	otherDirectory, otherBinding := fixture(t.TempDir())
	otherBinding.InstanceID = uuid.NewString()
	other, err := Open(otherDirectory, otherBinding)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.ID == selected[0].ID {
		t.Fatal("independent instances shared a session")
	}
}

func TestSessionProcessDeathReleasesOwnershipWithoutChangingIdentity(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionProcessHelper$")
	cmd.Env = append(os.Environ(), "CLAUDE_SESSION_PROCESS_HELPER="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !validID(strings.TrimSpace(line)) {
		t.Fatalf("helper did not reserve a session: %v", err)
	}
	directory, binding := fixture(root)
	if session, err := Open(directory, binding); err == nil {
		_ = session.Close()
		t.Fatal("live process ownership was ignored")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	resumed, err := Open(directory, binding)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.ID != strings.TrimSpace(line) || resumed.Transcript == "" {
		t.Fatal("process death lost or replaced the native session binding")
	}
}

func TestSessionProcessHelper(t *testing.T) {
	root := os.Getenv("CLAUDE_SESSION_PROCESS_HELPER")
	if root == "" {
		return
	}
	directory, binding := fixture(root)
	session, err := Open(directory, binding)
	if err != nil {
		os.Exit(2)
	}
	if _, err := history(binding, session.ID); err != nil {
		os.Exit(2)
	}
	fmt.Println(session.ID)
	for {
		time.Sleep(time.Hour)
	}
}
