package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	gatewayv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/gateway/v1"
	"github.com/agynio/agynd-cli/internal/config"
	"github.com/agynio/agynd-cli/internal/platform"
	claude "github.com/agynio/claude-sdk-go"
	"github.com/google/uuid"
	"google.golang.org/grpc"
)

type journalInboxClient struct {
	gatewayv1.AgentsGatewayClient
	err  error
	acks int
}

func (f *journalInboxClient) AckInboxItems(context.Context, *agentsv1.AckInboxItemsRequest, ...grpc.CallOption) (*agentsv1.AckInboxItemsResponse, error) {
	f.acks++
	return &agentsv1.AckInboxItemsResponse{}, f.err
}

func journalDaemon(client *fakeClaudeClient, inbox *journalInboxClient) *Daemon {
	return &Daemon{sdk: SDKClaude, cfg: config.Config{AgentInstanceID: uuid.MustParse(testAgentInstanceID)},
		claude: client, agentInbox: platform.NewAgents(inbox), agent: &agentsv1.Agent{}, mcpReady: true}
}

func TestInboxJournalAckLossDoesNotRepeatAgent(t *testing.T) {
	t.Setenv("AGYN_INBOX_JOURNAL_DIR", filepath.Join(t.TempDir(), "journal"))
	t.Setenv("AGYN_INBOX_CONTROL_FILE", "")
	client := &fakeClaudeClient{result: &claude.TurnResult{Response: "done"}}
	inbox := &journalInboxClient{err: errors.New("ack response lost")}
	d := journalDaemon(client, inbox)
	message := platform.Message{ID: "message", InboxItemID: "item", ThreadID: "thread", Body: "do work"}
	if err := d.handleMessage(context.Background(), message); err == nil {
		t.Fatal("expected ack failure")
	}
	inbox.err = nil
	// The same process can retry; a new process can retry after another lost ACK.
	for _, daemon := range []*Daemon{d, journalDaemon(client, inbox)} {
		if err := daemon.handleMessage(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	if client.turnCalls != 1 || inbox.acks != 3 {
		t.Fatalf("turns=%d acks=%d", client.turnCalls, inbox.acks)
	}
}

func TestInboxJournalAgentFailureNeverAutomaticallyRetries(t *testing.T) {
	t.Setenv("AGYN_INBOX_JOURNAL_DIR", filepath.Join(t.TempDir(), "journal"))
	t.Setenv("AGYN_INBOX_CONTROL_FILE", "")
	client := &fakeClaudeClient{err: errors.New("connection lost after tool side effect")}
	inbox := &journalInboxClient{}
	d := journalDaemon(client, inbox)
	message := platform.Message{ID: "message", InboxItemID: "item", ThreadID: "thread", Body: "do work"}
	if err := d.handleMessage(context.Background(), message); err == nil {
		t.Fatal("expected agent error")
	}
	for _, daemon := range []*Daemon{d, journalDaemon(client, inbox)} {
		if err := daemon.handleMessage(context.Background(), message); !isTerminalAgentProcessingError(err) {
			t.Fatalf("expected terminal reconciliation error: %v", err)
		}
	}
	if client.turnCalls != 1 || inbox.acks != 0 {
		t.Fatalf("turns=%d acks=%d", client.turnCalls, inbox.acks)
	}
}

func TestInboxControlWithoutJournalBlocksEverySDK(t *testing.T) {
	t.Setenv("AGYN_INBOX_JOURNAL_DIR", "")
	t.Setenv("AGYN_INBOX_CONTROL_FILE", "/missing-control.json")
	for _, sdk := range []string{SDKCodex, SDKAgn, SDKClaude} {
		d := &Daemon{sdk: sdk}
		if err := d.handleMessage(context.Background(), platform.Message{}); !isTerminalAgentProcessingError(err) {
			t.Fatalf("%s did not fail closed: %v", sdk, err)
		}
	}
}
