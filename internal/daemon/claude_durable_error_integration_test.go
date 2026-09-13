package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	"github.com/agynio/agynd-cli/internal/claudebridge"
	"github.com/agynio/agynd-cli/internal/platform"
	claude "github.com/agynio/claude-sdk-go"
)

// These fake-client cases exercise the lab combination of three focused patches.
func TestClaudeDurableFailureCannotBecomeCompletionAfterReplacement(t *testing.T) {
	for _, mode := range []string{"api-error", "nil-result", "wrong-session"} {
		t.Run(mode, func(t *testing.T) {
			cfg, state, _ := persistentClaudeFixture(t)
			journal := filepath.Join(t.TempDir(), "journal")
			t.Setenv("AGYN_INBOX_JOURNAL_DIR", journal)
			t.Setenv("AGYN_INBOX_CONTROL_FILE", "")
			session, err := prepareClaudeSession(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			status := 401
			result := &claude.TurnResult{SessionID: session.ID, IsError: true,
				Subtype: "success", TerminalReason: "api_error", APIErrorStatus: &status,
				Response: "sensitive upstream content"}
			want := errClaudeTurnFailed
			switch mode {
			case "nil-result":
				result = nil
			case "wrong-session":
				result.IsError = false
				result.SessionID = "different-session"
				want = errClaudeSessionMismatch
			}
			client, inbox, threads := &fakeClaudeClient{result: result}, &journalInboxClient{}, &fakeClaudeThreadsClient{}
			makeDaemon := func(selected *claudebridge.Session) *Daemon {
				return &Daemon{cfg: cfg, sdk: SDKClaude, claude: client, claudeSession: selected,
					claudeReady: true, agentInbox: platform.NewAgents(inbox), threads: platform.NewThreads(threads),
					agent: &agentsv1.Agent{FinalMessage: agentsv1.AgentFinalMessage_AGENT_FINAL_MESSAGE_DEFAULT_THREAD}}
			}
			d := makeDaemon(session)
			defer d.Close()
			message := platform.Message{ID: "message", InboxItemID: "item", ThreadID: "thread", Body: "sensitive task prompt"}
			err = d.handleMessage(context.Background(), message)
			if !errors.Is(err, want) || !isTerminalAgentProcessingError(err) {
				t.Fatalf("failed turn was not terminal: %v", err)
			}
			if strings.Contains(err.Error(), "sensitive") || (mode == "api-error" && !strings.Contains(err.Error(), "api_status=401")) {
				t.Fatalf("incorrect failure diagnostic: %v", err)
			}
			assertPending := func() {
				t.Helper()
				directory := filepath.Join(journal, cfg.AgentInstanceID.String())
				entries, err := os.ReadDir(directory)
				if err != nil || len(entries) != 1 {
					t.Fatalf("expected one durable inbox record: %v", err)
				}
				body, err := os.ReadFile(filepath.Join(directory, entries[0].Name()))
				if err != nil {
					t.Fatal(err)
				}
				var record struct {
					State     string `json:"state"`
					MessageID string `json:"message_id"`
				}
				if err := json.Unmarshal(body, &record); err != nil || record.State != "pending" || record.MessageID != message.ID {
					t.Fatal("failed turn did not remain pending")
				}
				if strings.Contains(string(body), "sensitive") {
					t.Fatal("inbox journal stored message content")
				}
			}
			assertPending()
			assertBlocked := func(next *Daemon) {
				t.Helper()
				err := next.handleMessage(context.Background(), message)
				var blocked *terminalInboxError
				if !errors.As(err, &blocked) || !isTerminalAgentProcessingError(err) {
					t.Fatalf("ambiguous turn could be dispatched again: %v", err)
				}
			}
			assertBlocked(d)
			// A valid native transcript permits session selection, not turn replay.
			nativeClaudeFixture(t, state, cfg.WorkDir, session.ID)
			d.Close()
			restored, err := prepareClaudeSession(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if restored.ID != session.ID || restored.Transcript == "" {
				t.Fatal("replacement did not recover the same native identity")
			}
			replacement := makeDaemon(restored)
			defer replacement.Close()
			assertBlocked(replacement)
			assertPending()
			if client.turnCalls != 1 || inbox.acks != 0 || len(threads.sendRequests) != 0 || len(threads.ackRequests) != 0 {
				t.Fatal("failed turn was rerun, published or acknowledged")
			}
		})
	}
}
