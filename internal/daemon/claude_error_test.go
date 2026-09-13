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

func TestClaudeErrorDiagnosticsAreAllowlistedAndTerminal(t *testing.T) {
	status := func(value int) *int { return &value }
	for _, tt := range []struct {
		name   string
		result *claude.TurnResult
		want   string
	}{
		{name: "legacy", result: &claude.TurnResult{IsError: true}, want: "result_subtype=unknown, terminal_reason=unknown, api_status=0"},
		{name: "API failure with success subtype", result: &claude.TurnResult{IsError: true, Subtype: "success", TerminalReason: "api_error", APIErrorStatus: status(401)}, want: "result_subtype=success, terminal_reason=api_error, api_status=401"},
		{name: "execution failure", result: &claude.TurnResult{IsError: true, Subtype: "error_during_execution"}, want: "result_subtype=error_during_execution, terminal_reason=unknown, api_status=0"},
		{name: "turn budget", result: &claude.TurnResult{IsError: true, Subtype: "error_max_turns", TerminalReason: "max_turns"}, want: "result_subtype=error_max_turns, terminal_reason=max_turns, api_status=0"},
		{name: "unsafe strings", result: &claude.TurnResult{IsError: true, Subtype: "sensitive\nBearer secret", TerminalReason: "sensitive body", APIErrorStatus: status(-401)}, want: "result_subtype=unknown, terminal_reason=unknown, api_status=0"},
		{name: "invalid status", result: &claude.TurnResult{IsError: true, APIErrorStatus: status(600)}, want: "api_status=0"},
		{name: "nonerror HTTP status", result: &claude.TurnResult{IsError: true, APIErrorStatus: status(200)}, want: "api_status=0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.result.Response = "sensitive response body"
			tt.result.StopReason = "sensitive stop reason"
			tt.result.SessionID = "sensitive session identity"
			client := &fakeClaudeClient{result: tt.result}
			threads := &fakeClaudeThreadsClient{}
			daemon := &Daemon{sdk: SDKClaude, claude: client, threads: platform.NewThreads(threads),
				agent: &agentsv1.Agent{FinalMessage: agentsv1.AgentFinalMessage_AGENT_FINAL_MESSAGE_DEFAULT_THREAD}}
			err := daemon.handleClaudeMessage(context.Background(), platform.Message{ID: "message", ThreadID: "thread", Body: "work"})
			if !errors.Is(err, errClaudeTurnFailed) || !isTerminalAgentProcessingError(err) {
				t.Fatalf("diagnostic changed terminal failure: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("missing diagnostic %q in %v", tt.want, err)
			}
			if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "secret") {
				t.Fatal("upstream content escaped into the error")
			}
			if client.turnCalls != 1 || len(threads.sendRequests) != 0 || len(threads.ackRequests) != 0 {
				t.Fatal("failed turn published or acknowledged")
			}
		})
	}
}
