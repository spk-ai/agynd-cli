package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/agynio/agynd-cli/internal/claudebridge"
	"github.com/agynio/agynd-cli/internal/config"
	"github.com/agynio/agynd-cli/internal/platform"
	"github.com/agynio/agynd-cli/internal/subscriber"
	"github.com/agynio/agynd-cli/internal/tracing"
	claude "github.com/agynio/claude-sdk-go"
)

var errClaudeTurnFailed = errors.New("Claude returned an error result; reconciliation required")

// newClaudeDaemon starts the CLI after init scripts and MCP readiness,
// selecting either the reserved SessionID or the verified transcript via
// Resume. It never falls back to a new conversation if persistent selection
// fails.
//
// @see claude-sdk::options
// @see internal/claudebridge/session.go
func newClaudeDaemon(ctx context.Context, cfg config.Config, version string, session *claudebridge.Session) (*Daemon, error) {
	// version is unused: the Claude SDK has no client-info metadata.
	_ = version

	setup, updatedCfg, err := connectPlatform(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cfg = updatedCfg

	if _, err := writeSkills(cfg.SDK, setup.skills); err != nil {
		_ = setup.gatewayConn.Close()
		return nil, err
	}

	if err := writeClaudeSettings(claudeBaseURL(cfg.LLMBaseURL), cfg.LLMAPIToken, cfg.MCPServers, cfg.LLMNative); err != nil {
		_ = setup.gatewayConn.Close()
		return nil, err
	}

	if err := runInitScripts(ctx, setup.agents, cfg.AgentID.String(), cfg.EnvironmentID, cfg.WorkDir); err != nil {
		_ = setup.gatewayConn.Close()
		return nil, err
	}

	// Claude Code connects to its MCP servers only when it starts: a sidecar
	// still starting then stays missing for the whole session. The servers
	// must answer an MCP initialize before the CLI is started.
	if err := waitForMCPServers(ctx, cfg.MCPServers, mcpReadyTimeout); err != nil {
		_ = setup.gatewayConn.Close()
		return nil, err
	}

	// What the platform hands the agent CLI is exported here; what the CLI did
	// with it is exported by the trace hook, into the trace opened here and
	// handed to it below.
	tracingExporter, err := tracing.NewExporter(tracing.Config{
		Address:    cfg.TracingAddress,
		WorkloadID: cfg.WorkloadID,
	})
	if err != nil {
		_ = setup.gatewayConn.Close()
		return nil, err
	}

	options := claude.Options{
		BinaryPath: cfg.AgentBinary,
		WorkDir:    cfg.WorkDir,
		Env: []string{
			"PATH=" + agentPathValue(),
			"LD_LIBRARY_PATH=/agyn/bin/lib",
			// The hook is told which transcript it is being handed rather
			// than sniffing the file, and where to export what it reads.
			traceHookFormatEnv + "=" + traceFormatClaude,
			traceHookAddressEnv + "=" + cfg.TracingAddress,
			traceHookTraceEnv + "=" + traceHookTraceID(cfg.WorkloadID),
			traceHookWorkloadEnv + "=" + cfg.WorkloadID,
			"IS_SANDBOX=1",
		},
	}
	if session != nil {
		if session.Transcript != "" {
			options.Resume = session.Transcript
		} else {
			options.SessionID = session.ID
		}
	}
	if model := claudeModel(cfg, setup.agent.GetModel()); model != "" {
		options.Model = model
		if !cfg.LLMNative {
			// A platform model is a UUID the vendor has never heard of, so the
			// CLI has to be told it is a model at all. A native model name is
			// the vendor's own and needs no such help.
			options.Env = append(options.Env,
				"ANTHROPIC_MODEL="+model,
				"ANTHROPIC_CUSTOM_MODEL_OPTION="+model,
			)
		}
	}
	// The agent's own prompt, not its role: the role is an internal label and
	// naming it here left the model with a single word for instructions.
	agentConfig, err := parseAgentConfiguration(setup.agent.GetConfiguration())
	if err != nil {
		_ = tracingExporter.Close()
		_ = setup.gatewayConn.Close()
		return nil, err
	}
	if prompt := strings.TrimSpace(agentConfig.SystemPrompt); prompt != "" {
		options.SystemPrompt = prompt
	}
	claudeClient, err := claude.Start(ctx, options)
	if err != nil {
		_ = tracingExporter.Close()
		_ = setup.gatewayConn.Close()
		return nil, err
	}

	return &Daemon{
		cfg:           cfg,
		sdk:           SDKClaude,
		gatewayConn:   setup.gatewayConn,
		threads:       setup.threads,
		agents:        setup.agents,
		agentInbox:    setup.agentInbox,
		runners:       setup.runners,
		subscriber:    subscriber.New(setup.notifications, cfg.ThreadID),
		consumer:      platform.NewInboxConsumer(setup.agentInbox, pageSize, pageTimeout),
		claude:        claudeClient,
		claudeSession: session,
		agent:         setup.agent,
		tracing:       tracingExporter,
		mcpReady:      true,
	}, nil
}

func (d *Daemon) ensureClaudeReady(ctx context.Context) error {
	d.claudeReadyMu.Lock()
	defer d.claudeReadyMu.Unlock()
	if d.claudeReady {
		return nil
	}
	if err := d.ensureMCPReady(ctx); err != nil {
		return err
	}
	d.claudeReady = true
	return nil
}

// handleClaudeMessage rejects nil/error results and persistent session mismatches
// before reply publication or inbox ACK. IsError is authoritative even with a
// success subtype and no Go error; these failures are terminal in Run's sync loop.
//
// @see claude-sdk::client
func (d *Daemon) handleClaudeMessage(ctx context.Context, message platform.Message) error {
	threadID := strings.TrimSpace(message.ThreadID)
	if threadID == "" {
		return fmt.Errorf("message %s missing thread id", message.ID)
	}
	inputText, err := buildInput(message)
	if err != nil {
		return err
	}
	if err := d.ensureClaudeReady(ctx); err != nil {
		return fmt.Errorf("prepare claude MCP servers: %w", err)
	}
	result, err := d.claude.Turn(ctx, claude.TurnParams{Prompt: inputText}, nil)
	if err != nil {
		return operationError(
			opClaudeTurn,
			0,
			fmt.Errorf("run claude turn for message %s on thread %s: %w", message.ID, threadID, err),
		)
	}
	if result == nil || result.IsError {
		return operationError(opClaudeTurn, 0, claudeTurnFailure(result))
	}
	if d.claudeSession != nil && result.SessionID != d.claudeSession.ID {
		return operationError(opClaudeTurn, 0, errClaudeSessionMismatch)
	}
	response := strings.TrimSpace(result.Response)
	if err := d.publishFinalMessage(ctx, SDKClaude, message, response); err != nil {
		return err
	}
	if err := d.ackMessage(ctx, message); err != nil {
		return err
	}
	return nil
}

// claudeTurnFailure exposes only allowlisted subtype/reason and HTTP status
// 400..599, defaulting to unknown/0. Response bodies, arbitrary stop reasons, and
// native session identifiers must not enter this diagnostic.
func claudeTurnFailure(result *claude.TurnResult) error {
	if result == nil {
		return errClaudeTurnFailed
	}
	subtype, reason, status := "unknown", "unknown", 0
	switch result.Subtype {
	case "success", "error_during_execution", "error_max_turns", "error_max_budget_usd", "error_max_structured_output_retries":
		subtype = result.Subtype
	}
	switch result.TerminalReason {
	case "api_error", "completed", "max_turns", "max_budget_usd", "max_structured_output_retries", "aborted_streaming", "aborted_tools":
		reason = result.TerminalReason
	}
	if result.APIErrorStatus != nil && *result.APIErrorStatus >= 400 && *result.APIErrorStatus <= 599 {
		status = *result.APIErrorStatus
	}
	return fmt.Errorf("%w (result_subtype=%s, terminal_reason=%s, api_status=%d)", errClaudeTurnFailed, subtype, reason, status)
}

// claudeModel picks what to pin the CLI to: the platform Model UUID the proxy
// resolves, or the vendor's own model name. Unset in native mode -- the common
// case -- leaves the CLI on its own default and its own picker.
func claudeModel(cfg config.Config, platformModel string) string {
	if cfg.LLMNative {
		return strings.TrimSpace(cfg.LLMModelName)
	}
	return strings.TrimSpace(platformModel)
}
