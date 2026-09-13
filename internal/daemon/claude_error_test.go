package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	"github.com/agynio/agynd-cli/internal/platform"
	claude "github.com/agynio/claude-sdk-go"
)

func TestClaudeErrorResultStopsBeforeReplyOrAck(t *testing.T) {
	for _, result := range []*claude.TurnResult{nil, {IsError: true, Response: "sensitive upstream error"}} {
		client := &fakeClaudeClient{result: result}
		threads := &fakeClaudeThreadsClient{}
		daemon := &Daemon{sdk: SDKClaude, claude: client, threads: platform.NewThreads(threads),
			agent: &agentsv1.Agent{FinalMessage: agentsv1.AgentFinalMessage_AGENT_FINAL_MESSAGE_DEFAULT_THREAD}}
		err := daemon.handleClaudeMessage(context.Background(), platform.Message{ID: "message", ThreadID: "thread", Body: "work"})
		if !errors.Is(err, errClaudeTurnFailed) || !isTerminalAgentProcessingError(err) {
			t.Fatalf("error result could be retried: %v", err)
		}
		if strings.Contains(err.Error(), "sensitive") {
			t.Fatal("upstream error body escaped into the processing error")
		}
		if client.turnCalls != 1 || len(threads.sendRequests) != 0 || len(threads.ackRequests) != 0 {
			t.Fatal("failed turn published a reply or acknowledged the inbox")
		}
	}
}
