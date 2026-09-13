package inboxjournal

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agynio/agynd-cli/internal/platform"
)

const testInstance = "1c2f0f4e-8b9d-4a5b-9b3c-1d2e3f4a5b6c"

var testMessage = platform.Message{ID: "message-1", InboxItemID: "item-1", ThreadID: "thread-1", SenderID: "sender-1", Body: "perform an action"}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openTestJournal(t *testing.T, directory, control string) *Journal {
	t.Helper()
	j, err := Open(directory, testInstance, control)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func writeControl(t *testing.T, path string, control Control) {
	t.Helper()
	data, err := json.Marshal(control)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPendingBlocksRestartAndCompletionRetriesOnlyAck(t *testing.T) {
	dir := privateDir(t)
	j := openTestJournal(t, dir, "")
	if execute, err := j.Begin(testMessage); err != nil || !execute {
		t.Fatalf("first attempt: %t %v", execute, err)
	}
	restarted := openTestJournal(t, dir, "")
	if execute, err := restarted.Begin(testMessage); err == nil || execute {
		t.Fatalf("pending attempt must not replay: %t %v", execute, err)
	}
	if err := j.Complete(testMessage); err != nil {
		t.Fatal(err)
	}
	if execute, err := restarted.Begin(testMessage); err != nil || execute {
		t.Fatalf("completed attempt must only ack: %t %v", execute, err)
	}
	mutated := testMessage
	mutated.Body = "different action"
	if _, err := restarted.Begin(mutated); err == nil {
		t.Fatal("content identity mismatch accepted")
	}
	mutated = testMessage
	mutated.InboxItemID = "different-item"
	if _, err := restarted.Begin(mutated); err == nil {
		t.Fatal("inbox identity mismatch accepted")
	}
}

func TestControlRequiresExactMessageAndExplicitRetirement(t *testing.T) {
	dir := privateDir(t)
	control := filepath.Join(t.TempDir(), "control.json")
	writeControl(t, control, Control{Version: 1, InstanceID: testInstance, AllowedMessageID: testMessage.ID})
	j := openTestJournal(t, dir, control)
	if _, err := j.Begin(testMessage); err != nil {
		t.Fatal(err)
	}
	next := testMessage
	next.ID, next.InboxItemID = "message-2", "item-2"
	if _, err := j.Begin(next); err == nil {
		t.Fatal("unexpected follow-up message accepted in the same workload")
	}
	writeControl(t, control, Control{Version: 1, InstanceID: testInstance, AllowedMessageID: next.ID, AckOnlyMessageIDs: []string{testMessage.ID, "never-started"}})
	recovered := openTestJournal(t, dir, control)
	if execute, err := recovered.Begin(testMessage); err != nil || execute {
		t.Fatalf("explicit retirement: %t %v", execute, err)
	}
	if err := recovered.Complete(testMessage); err != nil {
		t.Fatal(err)
	}
	expected, _ := recovered.expected(testMessage)
	value, err := recovered.read(expected)
	if err != nil || value.State != "ack_only" {
		t.Fatalf("retirement must not claim successful completion: %#v %v", value, err)
	}
	unstarted := testMessage
	unstarted.ID, unstarted.InboxItemID = "never-started", "item-3"
	if execute, err := recovered.Begin(unstarted); err != nil || execute {
		t.Fatalf("retire unstarted request: %t %v", execute, err)
	}
	if execute, err := recovered.Begin(next); err != nil || !execute {
		t.Fatalf("new authorized message: %t %v", execute, err)
	}
}

func TestInvalidJournalAndControlFailClosed(t *testing.T) {
	for _, content := range []string{
		`{}`, `{"version":1}`, `{"version":2}`, `[]`, `null`,
		`{"version":1,"instance_id":"` + testInstance + `","allowed_message_id":"m","extra":true}`,
		`{"version":1,"instance_id":"` + testInstance + `","allowed_message_id":"m","ack_only_message_ids":["m"]}`,
		`{"version":1,"instance_id":"` + testInstance + `","allowed_message_id":"m","ack_only_message_ids":["old","old"]}`,
	} {
		t.Run(content, func(t *testing.T) {
			control := filepath.Join(t.TempDir(), "control.json")
			if err := os.WriteFile(control, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(t.TempDir(), testInstance, control); err == nil {
				t.Fatal("invalid control accepted")
			}
		})
	}
	dir := privateDir(t)
	j := openTestJournal(t, dir, "")
	if err := j.Complete(testMessage); err == nil {
		t.Fatal("completion without intent accepted")
	}
	if err := os.WriteFile(j.path(testMessage.ID), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Begin(testMessage); err == nil {
		t.Fatal("corrupt record accepted")
	}
	if err := os.Remove(j.path(testMessage.ID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), j.path(testMessage.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Begin(testMessage); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := Open("relative", testInstance, ""); err == nil {
		t.Fatal("relative journal accepted")
	}
	if _, err := Open(t.TempDir(), "../other", ""); err == nil {
		t.Fatal("invalid instance accepted")
	}
	if _, err := Open(t.TempDir(), testInstance, filepath.Join(dir, "absent")); err == nil {
		t.Fatal("missing required control accepted")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, testInstance, ""); err == nil {
		t.Fatal("public journal directory accepted")
	}
}

func TestJournalProcessHelper(t *testing.T) {
	dir := os.Getenv("AGYN_JOURNAL_TEST_HELPER")
	if dir == "" {
		return
	}
	j, err := Open(dir, testInstance, "")
	if err != nil {
		os.Exit(3)
	}
	execute, err := j.Begin(testMessage)
	if err != nil || !execute {
		os.Exit(3)
	}
	if os.Getenv("AGYN_JOURNAL_TEST_CRASH") == "true" {
		if err := os.WriteFile(filepath.Join(dir, "side-effect"), []byte("once"), 0600); err != nil {
			os.Exit(4)
		}
		os.Exit(7) // No completion record or daemon cleanup.
	}
	os.Exit(0)
}

func TestMultipleProcessesCannotBothBegin(t *testing.T) {
	dir := privateDir(t)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestJournalProcessHelper$")
			cmd.Env = append(os.Environ(), "AGYN_JOURNAL_TEST_HELPER="+dir)
			results <- cmd.Run()
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected one durable begin, got %d", winners)
	}
}

func TestProcessDeathAfterSideEffectRequiresReconciliation(t *testing.T) {
	dir := privateDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestJournalProcessHelper$")
	cmd.Env = append(os.Environ(), "AGYN_JOURNAL_TEST_HELPER="+dir, "AGYN_JOURNAL_TEST_CRASH=true")
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
		t.Fatalf("expected simulated crash: %v", err)
	}
	if marker, err := os.ReadFile(filepath.Join(dir, "side-effect")); err != nil || string(marker) != "once" {
		t.Fatalf("side effect: %q %v", marker, err)
	}
	j := openTestJournal(t, dir, "")
	if execute, err := j.Begin(testMessage); err == nil || execute {
		t.Fatalf("crashed turn replayed: %t %v", execute, err)
	}
}
