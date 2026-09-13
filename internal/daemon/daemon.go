package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agnsdk "github.com/agynio/agn-sdk-go"
	agentsv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/agents/v1"
	gatewayv1 "github.com/agynio/agynd-cli/.gen/go/agynio/api/gateway/v1"
	"github.com/agynio/agynd-cli/internal/claudebridge"
	"github.com/agynio/agynd-cli/internal/codexbridge"
	"github.com/agynio/agynd-cli/internal/config"
	"github.com/agynio/agynd-cli/internal/inboxjournal"
	"github.com/agynio/agynd-cli/internal/platform"
	"github.com/agynio/agynd-cli/internal/subscriber"
	"github.com/agynio/agynd-cli/internal/tracing"
	"github.com/agynio/agynd-cli/internal/tracingproxy"
	claude "github.com/agynio/claude-sdk-go"
	codex "github.com/agynio/codex-sdk-go"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	pageSize              int32 = 100
	pageTimeout                 = 30 * time.Second
	turnStartTimeout            = 5 * time.Minute
	messagePublishTimeout       = 15 * time.Second
	messageAckTimeout           = 15 * time.Second
	mcpReadyTimeout             = 4 * time.Minute
	// codexHandshakeTimeout bounds the initialize exchange with the agent
	// process. Generous, because it also covers the binary starting up.
	codexHandshakeTimeout     = 2 * time.Minute
	llmReadyTimeout           = 120 * time.Second
	codexReadbackTimeout      = 10 * time.Second
	codexReadbackPollInterval = 250 * time.Millisecond
	syncRetryInitialBackoff   = 1 * time.Second
	syncRetryMaxBackoff       = 30 * time.Second
	opSyncPageFetch           = "sync_page_fetch"
	opCodexStartTurn          = "codex_start_turn"
	opMessagePublish          = "publish"
	opMessageAck              = "ack"
	opKeepaliveTouch          = "keepalive_touch"
	opCodexWaitTurnCompletion = "codex_wait_turn_completion"
	opCodexTurnResult         = "codex_turn_result"
	opAgnTurn                 = "agn_turn"
	opClaudeTurn              = "claude_turn"
	opProcessSignalShutdown   = "process_signal/shutdown"
	mcpLoopbackHost           = "127.0.0.1"
)

var retryableCodexErrorNotificationTerms = [...]string{
	"stream disconn",
	"stream disconnected",
	"connection reset",
	"connection refused",
	"connection closed",
	"EOF",
	"timeout",
	"temporary network failure",
}

const (
	SDKCodex  = "codex"
	SDKAgn    = "agn"
	SDKClaude = "claude"
)

const mcpProbeID = "agynd-mcp-ready"

type Daemon struct {
	cfg             config.Config
	sdk             string
	gatewayConn     platformConn
	threads         *platform.Threads
	agents          gatewayv1.AgentsGatewayClient
	agentInbox      *platform.Agents
	runners         runnersClient
	subscriber      messageSubscriber
	consumer        messageConsumer
	codex           codexClient
	mapping         *codexbridge.ThreadMapping
	mappingStore    *codexbridge.ThreadMappingStore
	tracker         *codexbridge.TurnTracker
	agn             *agnsdk.Client
	claude          claudeClient
	claudeSession   *claudebridge.Session
	claudeCloseOnce sync.Once
	agent           *agentsv1.Agent
	tracing         *tracing.Exporter
	tracingProxy    *tracingproxy.Proxy
	claudeReadyMu   sync.Mutex
	claudeReady     bool
	mcpReadyMu      sync.Mutex
	mcpReady        bool

	inboxJournal      *inboxjournal.Journal
	inboxJournalReady bool

	processing     atomic.Bool
	processingWake chan struct{}
	syncMu         sync.Mutex
}

type platformConn interface {
	Close() error
}

type codexClient interface {
	StartThread(ctx context.Context, params *codex.ThreadStartParams) (*codex.ThreadStartResponse, error)
	ResumeThread(ctx context.Context, params *codex.ThreadResumeParams) (*codex.ThreadResumeResponse, error)
	ReadThread(ctx context.Context, params *codex.ThreadReadParams) (*codex.ThreadReadResponse, error)
	StartTurn(ctx context.Context, params *codex.TurnStartParams) (*codex.TurnStartResponse, error)
	Close() error
}

type claudeClient interface {
	Turn(ctx context.Context, params claude.TurnParams, handler claude.EventHandler) (*claude.TurnResult, error)
	Close() error
}

type runnersClient interface {
	TouchWorkload(ctx context.Context, workloadID string) error
}

type messageSubscriber interface {
	Run(ctx context.Context) error
	Started() <-chan struct{}
	Wake() <-chan struct{}
}

type messageConsumer interface {
	Sync(ctx context.Context, participantID string, threadID string, handle func(platform.Message) error) error
}

type platformSetup struct {
	gatewayConn   platformConn
	threads       *platform.Threads
	notifications *platform.Notifications
	agents        gatewayv1.AgentsGatewayClient
	agentInbox    *platform.Agents
	runners       *platform.Runners
	agent         *agentsv1.Agent
	skills        []skill
}

func New(ctx context.Context, cfg config.Config, version string) (*Daemon, error) {
	var session *claudebridge.Session
	if cfg.SDK == SDKClaude {
		var err error
		session, err = prepareClaudeSession(cfg)
		if err != nil {
			return nil, err
		}
		// Ownership transfers only after a fully constructed daemon is returned.
		defer func() {
			if session != nil {
				_ = session.Close()
			}
		}()
	}
	if err := prepareAgentCLI(cfg); err != nil {
		return nil, err
	}
	if cfg.Mode == config.ModeHolder {
		return newHolderDaemon(ctx, cfg)
	}
	switch cfg.SDK {
	case SDKCodex:
		return newCodexDaemon(ctx, cfg, version)
	case SDKAgn:
		if cfg.LLMNative {
			// agn has no vendor of its own to be intercepted at: it calls the
			// platform LLM Proxy by a platform Model UUID, which a native
			// environment's agents do not carry.
			return nil, fmt.Errorf("the agn SDK is not supported in native LLM mode")
		}
		return newAgnDaemon(ctx, cfg, version)
	case SDKClaude:
		daemon, err := newClaudeDaemon(ctx, cfg, version, session)
		if err == nil {
			session = nil
		}
		return daemon, err
	default:
		return nil, fmt.Errorf("unknown sdk %q", cfg.SDK)
	}
}

// prepareAgentCLI writes what the agent CLI needs on disk before anything runs
// it -- in holder mode too, where nothing will except a person, by hand.
func prepareAgentCLI(cfg config.Config) error {
	// Which credential a CLI needs on disk follows from the CLI, so it is
	// derived here rather than delivered: only this knows which one it runs.
	if cfg.SDK == SDKCodex {
		if cfg.LLMNative {
			if err := writeCodexAuth(time.Now()); err != nil {
				return err
			}
		}
		// The agent path writes config.toml later instead, once the platform
		// has resolved MCP ports into it.
		if cfg.Mode != config.ModeHolder {
			return nil
		}
		// Holder still needs it: the transport it settles is what keeps codex
		// off a WebSocket the proxy cannot terminate, and a sandbox is where
		// someone starts codex by hand.
		_, err := writeCodexConfig(cfg)
		return err
	}
	if cfg.SDK != SDKClaude {
		return nil
	}
	if err := writeClaudeState(); err != nil {
		return err
	}
	// The agent path writes settings.json later instead, once the platform has
	// resolved MCP ports into the config.
	if cfg.Mode != config.ModeHolder {
		return nil
	}
	return writeClaudeSettings(claudeBaseURL(cfg.LLMBaseURL), cfg.LLMAPIToken, cfg.MCPServers, cfg.LLMNative)
}

func newHolderDaemon(ctx context.Context, cfg config.Config) (*Daemon, error) {
	d := &Daemon{
		cfg: cfg,
		sdk: config.ModeHolder,
	}
	// A holder names no agent, so it resolves none: the environment's init
	// scripts are the whole of what it fetches. Without an environment there is
	// nothing to ask for, and the gateway is left alone.
	if strings.TrimSpace(cfg.EnvironmentID) == "" {
		return d, nil
	}
	conn, err := connectHolderGateway(ctx, cfg)
	if err != nil {
		return nil, err
	}
	d.gatewayConn = conn
	d.agents = gatewayv1.NewAgentsGatewayClient(conn)
	return d, nil
}

// connectHolderGateway dials the gateway on the same schedule agent mode uses.
// It cannot reuse connectPlatform: that resolves the agent and its skills, and
// a sandbox has neither.
func connectHolderGateway(ctx context.Context, cfg config.Config) (*grpc.ClientConn, error) {
	var lastErr error
	for i, delay := range platformConnectBackoff {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := platform.DialGateway(cfg.GatewayAddress)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		log.Printf(
			"holder gateway dial attempt %d/%d failed: %v; retrying in %s",
			i+1,
			len(platformConnectBackoff)+1,
			err,
			delay,
		)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := platform.DialGateway(cfg.GatewayAddress)
	if err != nil {
		return nil, fmt.Errorf(
			"holder gateway dial failed after %d attempts: %w (previous: %v)",
			len(platformConnectBackoff)+1,
			err,
			lastErr,
		)
	}
	return conn, nil
}

var platformConnectBackoff = []time.Duration{
	1 * time.Second,
	1 * time.Second,
	2 * time.Second,
	3 * time.Second,
	5 * time.Second,
	5 * time.Second,
	8 * time.Second,
	10 * time.Second,
	15 * time.Second,
	15 * time.Second,
	15 * time.Second,
}

func connectPlatform(ctx context.Context, cfg config.Config) (*platformSetup, config.Config, error) {
	backoff := platformConnectBackoff
	var lastErr error
	for i, delay := range backoff {
		if err := ctx.Err(); err != nil {
			return nil, config.Config{}, err
		}
		setup, updatedCfg, err := tryConnectPlatform(ctx, cfg)
		if err == nil {
			return setup, updatedCfg, nil
		}
		lastErr = err
		log.Printf(
			"platform connect attempt %d/%d failed: %v; retrying in %s",
			i+1,
			len(backoff)+1,
			err,
			delay,
		)
		select {
		case <-ctx.Done():
			return nil, config.Config{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, config.Config{}, err
	}
	setup, updatedCfg, err := tryConnectPlatform(ctx, cfg)
	if err != nil {
		return nil, config.Config{}, fmt.Errorf(
			"platform connect failed after %d attempts: %w (previous: %v)",
			len(backoff)+1,
			err,
			lastErr,
		)
	}
	return setup, updatedCfg, nil
}

func tryConnectPlatform(ctx context.Context, cfg config.Config) (*platformSetup, config.Config, error) {
	gatewayConn, err := platform.DialGateway(cfg.GatewayAddress)
	if err != nil {
		return nil, config.Config{}, fmt.Errorf("dial gateway: %w", err)
	}

	threadsGateway := gatewayv1.NewThreadsGatewayClient(gatewayConn)
	notificationsGateway := gatewayv1.NewNotificationsGatewayClient(gatewayConn)
	agentsClient := gatewayv1.NewAgentsGatewayClient(gatewayConn)
	runnersGateway := gatewayv1.NewRunnersGatewayClient(gatewayConn)

	threadsClient := platform.NewThreads(threadsGateway)
	notificationsClient := platform.NewNotifications(notificationsGateway)
	agentInboxClient := platform.NewAgents(agentsClient)
	runnersClient := platform.NewRunners(runnersGateway)

	agentResp, err := agentsClient.GetAgent(ctx, &agentsv1.GetAgentRequest{Id: cfg.AgentID.String()})
	if err != nil {
		_ = gatewayConn.Close()
		return nil, config.Config{}, fmt.Errorf("get agent: %w", err)
	}
	agent := agentResp.GetAgent()
	if agent == nil {
		_ = gatewayConn.Close()
		return nil, config.Config{}, fmt.Errorf("agent not found")
	}

	skills, err := listSkills(ctx, agentsClient, cfg.AgentID.String())
	if err != nil {
		_ = gatewayConn.Close()
		return nil, config.Config{}, fmt.Errorf("list skills: %w", err)
	}

	// The Orchestrator assembles the sidecars, so it is what knows which MCPs a
	// workload has -- an agent's and its environment's alike -- and hands them
	// over as AGENT_MCP_SERVERS. Asking the Agents service again would re-derive
	// that list from the same source, and agynd holds no relation to the
	// environment that would let it.
	resolvedMCPs, err := resolveMCPServers(mcpDefinitionsFromServers(cfg.MCPServers), cfg.MCPServers, cfg.MCPPort)
	if err != nil {
		_ = gatewayConn.Close()
		return nil, config.Config{}, err
	}
	updatedCfg := cfg
	updatedCfg.MCPServers = resolvedMCPs
	updatedCfg.MCPPort = nil

	return &platformSetup{
		gatewayConn:   gatewayConn,
		threads:       threadsClient,
		notifications: notificationsClient,
		agents:        agentsClient,
		agentInbox:    agentInboxClient,
		runners:       runnersClient,
		agent:         agent,
		skills:        skills,
	}, updatedCfg, nil
}

func newCodexDaemon(ctx context.Context, cfg config.Config, version string) (*Daemon, error) {
	setup, updatedCfg, err := connectPlatform(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cfg = updatedCfg

	if _, err := writeSkills(cfg.SDK, setup.skills); err != nil {
		_ = setup.gatewayConn.Close()
		return nil, err
	}

	tracker := codexbridge.NewTurnTracker()
	bridge := codexbridge.New(tracker)
	threadsMapping := codexbridge.NewThreadMapping()

	// What the platform hands the agent CLI is exported here; what the CLI
	// did with it is exported by the trace hook, into the trace both derive
	// from the workload.
	tracingExporter, err := tracing.NewExporter(tracing.Config{
		Address:    cfg.TracingAddress,
		WorkloadID: cfg.WorkloadID,
	})
	if err != nil {
		_ = setup.gatewayConn.Close()
		return nil, err
	}
	codexHome, err := writeCodexConfig(cfg)
	if err != nil {
		_ = tracingExporter.Close()
		_ = setup.gatewayConn.Close()
		return nil, err
	}

	if err := runInitScripts(ctx, setup.agents, cfg.AgentID.String(), cfg.EnvironmentID, cfg.WorkDir); err != nil {
		_ = tracingExporter.Close()
		_ = setup.gatewayConn.Close()
		return nil, err
	}
	codexHomeValue := codexHomeEnv()
	mappingStore := codexMappingStore(codexHome, codexHomeValue)
	options := []codex.Option{
		codex.WithBinary(cfg.AgentBinary),
		codex.WithWorkDir(cfg.WorkDir),
		codex.WithEnv(codexEnv(cfg, codexHome, codexHomeValue)),
		codex.WithNotificationHandler(bridge),
		codex.WithApprovalHandler(codex.AutoApprovalHandler{}),
		codex.WithClientInfo("agynd", version),
	}
	// Deadlined, because the handshake is a request to a child process that can
	// fail to answer -- a codex that rejects its own config exits without a
	// reply -- and without one the daemon blocked here for good: no logs, no
	// inbox drain, a container that looks healthy and does nothing.
	codexCtx, cancelCodex := context.WithTimeout(ctx, codexHandshakeTimeout)
	codexClient, err := newCodexClient(codexCtx, cfg, options...)
	cancelCodex()
	if err != nil {
		log.Printf("start codex: %v", err)
		_ = tracingExporter.Close()
		_ = setup.gatewayConn.Close()
		return nil, err
	}

	return &Daemon{
		cfg:          cfg,
		sdk:          SDKCodex,
		gatewayConn:  setup.gatewayConn,
		threads:      setup.threads,
		agents:       setup.agents,
		agentInbox:   setup.agentInbox,
		runners:      setup.runners,
		subscriber:   subscriber.New(setup.notifications, cfg.ThreadID),
		consumer:     platform.NewInboxConsumer(setup.agentInbox, pageSize, pageTimeout),
		codex:        codexClient,
		mapping:      threadsMapping,
		mappingStore: mappingStore,
		tracker:      tracker,
		agent:        setup.agent,
		tracing:      tracingExporter,
	}, nil
}

func (d *Daemon) Close() {
	if d.codex != nil {
		_ = d.codex.Close()
	}
	if d.agn != nil {
		_ = d.agn.Close()
	}
	d.claudeCloseOnce.Do(func() {
		if d.claude != nil {
			if err := d.claude.Close(); err != nil {
				log.Printf("Claude close failed; retain session lock until daemon exit: %v", err)
				return
			}
		}
		if d.claudeSession != nil {
			_ = d.claudeSession.Close()
		}
	})
	if d.tracing != nil {
		_ = d.tracing.Close()
	}
	if d.tracingProxy != nil {
		d.tracingProxy.Close()
	}
	if d.gatewayConn != nil {
		_ = d.gatewayConn.Close()
	}
}

func (d *Daemon) Run(ctx context.Context) error {
	// Before either mode. A sandbox is the workload persistent shells exist
	// for, but the server is cheap and the alternative is a second place where
	// a shell's environment could differ.
	startShellServer(ctx)

	if d.sdk == config.ModeHolder {
		return runHolder(ctx, d.agents, d.cfg.EnvironmentID, d.cfg.WorkDir)
	}
	d.ensureProcessingWake()
	go d.runKeepalive(ctx)

	go func() {
		if err := d.subscriber.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("subscriber stopped: %v", err)
		}
	}()
	select {
	case <-ctx.Done():
		return operationError(opProcessSignalShutdown, 0, ctx.Err())
	case <-d.subscriber.Started():
	}

	backoff := syncRetryInitialBackoff
	var retryTimer *time.Timer
	var retry <-chan time.Time
	defer func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
	}()
	scheduleRetry := func(delay time.Duration) {
		if retryTimer == nil {
			retryTimer = time.NewTimer(delay)
		} else {
			if !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
			retryTimer.Reset(delay)
		}
		retry = retryTimer.C
	}
	clearRetry := func() {
		if retryTimer != nil {
			if !retryTimer.Stop() {
				select {
				case <-retryTimer.C:
				default:
				}
			}
		}
		retry = nil
		backoff = syncRetryInitialBackoff
	}
	scheduleFailureRetry := func(operation string, err error) {
		delay := backoff
		log.Printf("%s failed: %v; retrying in %s", operation, err, delay)
		backoff = nextSyncRetryBackoff(backoff)
		scheduleRetry(delay)
	}
	handleSyncFailure := func(operation string, err error) error {
		if isTerminalAgentProcessingError(err) {
			log.Printf("%s terminal agent processing failure: %v", operation, err)
			return err
		}
		scheduleFailureRetry(operation, err)
		return nil
	}

	if err := d.syncMessages(ctx); err != nil {
		if terminalErr := handleSyncFailure("initial sync messages", err); terminalErr != nil {
			return terminalErr
		}
	} else {
		clearRetry()
	}

	for {
		select {
		case <-ctx.Done():
			return operationError(opProcessSignalShutdown, 0, ctx.Err())
		case <-d.subscriber.Wake():
			if err := d.syncMessages(ctx); err != nil {
				if terminalErr := handleSyncFailure("sync messages", err); terminalErr != nil {
					return terminalErr
				}
			} else {
				clearRetry()
			}
		case <-retry:
			if err := d.syncMessages(ctx); err != nil {
				if terminalErr := handleSyncFailure("sync messages retry", err); terminalErr != nil {
					return terminalErr
				}
			} else {
				clearRetry()
			}
		}
	}
}

type operationFailure struct {
	op      string
	timeout time.Duration
	err     error
}

type terminalCodexTurnError struct {
	err error
}

func (e *terminalCodexTurnError) Error() string {
	return e.err.Error()
}

func (e *terminalCodexTurnError) Unwrap() error {
	return e.err
}

func terminalCodexTurn(err error) error {
	return &terminalCodexTurnError{err: err}
}

func (e *operationFailure) Error() string {
	if errors.Is(e.err, context.DeadlineExceeded) {
		if e.timeout > 0 {
			return fmt.Sprintf("%s timed out after %s: %v", e.op, e.timeout, e.err)
		}
		return fmt.Sprintf("%s timed out: %v", e.op, e.err)
	}
	if errors.Is(e.err, context.Canceled) {
		return fmt.Sprintf("%s canceled: %v", e.op, e.err)
	}
	if e.timeout > 0 {
		return fmt.Sprintf("%s failed (timeout %s): %v", e.op, e.timeout, e.err)
	}
	return fmt.Sprintf("%s failed: %v", e.op, e.err)
}

func (e *operationFailure) Unwrap() error {
	return e.err
}

func operationError(op string, timeout time.Duration, err error) error {
	if err == nil {
		return nil
	}
	return &operationFailure{op: op, timeout: timeout, err: err}
}

func isTerminalAgentProcessingError(err error) bool {
	var terminalErr *terminalCodexTurnError
	var inboxErr *terminalInboxError
	return errors.As(err, &terminalErr) || errors.As(err, &inboxErr) || errors.Is(err, errClaudeSessionMismatch)
}

func isRetryableCodexErrorNotification(err error) bool {
	var notificationErr *codexbridge.ErrorNotificationError
	if !errors.As(err, &notificationErr) {
		return false
	}
	normalizedMessage := strings.ToLower(strings.TrimSpace(notificationErr.Message))
	for _, retryableTerm := range retryableCodexErrorNotificationTerms {
		if strings.Contains(normalizedMessage, strings.ToLower(retryableTerm)) {
			return true
		}
	}
	return false
}

// codexWillRetryTurn reports whether codex is retrying the turn itself. It
// answers for the notification it is given, so the caller decides what a turn
// codex has stopped retrying means for the class of failure it is.
func codexWillRetryTurn(err error) bool {
	var notificationErr *codexbridge.ErrorNotificationError
	return errors.As(err, &notificationErr) && notificationErr.WillRetry
}

func operationContextErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		return ctxErr
	}
	return err
}

// publishFinalMessage delivers the text an agent CLI produced at the end of a
// turn. Whether that text is a deliverable at all is the class's to say, and
// where it goes is the instance's: the thread_id is left off the wire so
// Threads resolves the instance's default from the caller identity.
//
// Not the thread the message arrived on. An agent woken by a reply on a
// sub-thread still owes its answer to the thread it was created to serve.
//
// Nothing to say and nowhere to say it are both ordinary: an agent that
// already sent explicitly has an empty final text, and an instance whose class
// asked for no default has no destination. Both log and carry on, because
// failing here would leave the item unacked and the turn repeating forever.
func (d *Daemon) publishFinalMessage(ctx context.Context, sdk string, message platform.Message, response string) error {
	if d.agent.GetFinalMessage() != agentsv1.AgentFinalMessage_AGENT_FINAL_MESSAGE_DEFAULT_THREAD {
		return nil
	}
	response = strings.TrimSpace(response)
	if response == "" {
		log.Printf("final message: %s turn produced no text for message %s; nothing to post", sdk, message.ID)
		return nil
	}
	publishCtx, cancel := context.WithTimeout(ctx, messagePublishTimeout)
	_, err := d.threads.SendMessage(publishCtx, "", d.selfID(), response, nil)
	err = operationContextErr(publishCtx, err)
	cancel()
	if status.Code(err) == codes.FailedPrecondition {
		log.Printf("final message: no default thread for message %s; not posting: %v", message.ID, err)
		return nil
	}
	if err != nil {
		return operationError(
			opMessagePublish,
			messagePublishTimeout,
			fmt.Errorf("publish %s final message for message %s: %w", sdk, message.ID, err),
		)
	}
	return nil
}

// selfID is the identity the daemon acts as: its agent instance once one is
// assigned, and the agent class for callers that predate instances.
func (d *Daemon) selfID() string {
	if d.cfg.AgentInstanceID != uuid.Nil {
		return d.cfg.AgentInstanceID.String()
	}
	return d.cfg.AgentID.String()
}

func (d *Daemon) ackMessage(ctx context.Context, message platform.Message) error {
	if d.inboxJournal != nil {
		if err := d.inboxJournal.Complete(message); err != nil {
			return &terminalInboxError{err: fmt.Errorf("persist inbox completion: %w", err)}
		}
	}
	ackCtx, cancel := context.WithTimeout(ctx, messageAckTimeout)
	var err error
	// An inbox item is acked on the inbox that delivered it; the thread ack
	// would leave the item outstanding and the instance would re-read it.
	if message.InboxItemID != "" && d.agentInbox != nil {
		err = d.agentInbox.AckInboxItems(ackCtx, d.cfg.AgentInstanceID.String(), []string{message.InboxItemID})
	} else {
		err = d.threads.AckMessages(ackCtx, d.cfg.AgentID.String(), []string{message.ID})
	}
	err = operationContextErr(ackCtx, err)
	cancel()
	if err != nil {
		return operationError(
			opMessageAck,
			messageAckTimeout,
			fmt.Errorf("ack message %s: %w", message.ID, err),
		)
	}
	return nil
}

func (d *Daemon) ensureProcessingWake() {
	if d.processingWake == nil {
		d.processingWake = make(chan struct{}, 1)
	}
}

func (d *Daemon) signalProcessingStarted() {
	if d.processingWake == nil {
		return
	}
	select {
	case d.processingWake <- struct{}{}:
	default:
	}
}

func nextSyncRetryBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return syncRetryInitialBackoff
	}
	next := current * 2
	if next > syncRetryMaxBackoff {
		return syncRetryMaxBackoff
	}
	return next
}

func (d *Daemon) syncMessages(ctx context.Context) error {
	d.syncMu.Lock()
	d.processing.Store(true)
	d.signalProcessingStarted()
	defer func() {
		d.processing.Store(false)
		d.syncMu.Unlock()
	}()

	participantID := d.selfID()
	if err := d.consumer.Sync(ctx, participantID, d.cfg.ThreadID, func(message platform.Message) error {
		d.recordInvocation(ctx, message)
		if err := d.ensureMCPReady(ctx); err != nil {
			return fmt.Errorf("wait for MCP servers before processing message %s: %w", message.ID, err)
		}
		return d.handleMessage(ctx, message)
	}); err != nil {
		var pageFetchErr *platform.PageFetchError
		if !errors.As(err, &pageFetchErr) {
			return err
		}
		return operationError(
			opSyncPageFetch,
			pageTimeout,
			fmt.Errorf("sync unacked messages for participant %s thread %s: %w", participantID, d.cfg.ThreadID, pageFetchErr),
		)
	}
	return nil
}

func (d *Daemon) ensureMCPReady(ctx context.Context) error {
	d.mcpReadyMu.Lock()
	defer d.mcpReadyMu.Unlock()
	if d.mcpReady {
		return nil
	}
	if err := waitForMCPServers(ctx, d.cfg.MCPServers, mcpReadyTimeout); err != nil {
		return err
	}
	d.mcpReady = true
	return nil
}

func (d *Daemon) handleMessage(ctx context.Context, message platform.Message) error {
	journal, err := d.messageJournal()
	if err != nil {
		return &terminalInboxError{err: err}
	}
	if journal != nil {
		execute, err := journal.Begin(message)
		if err != nil {
			return &terminalInboxError{err: err}
		}
		if !execute {
			return d.ackMessage(ctx, message)
		}
	}
	switch d.sdk {
	case SDKCodex:
		return d.handleCodexMessage(ctx, message)
	case SDKAgn:
		return d.handleAgnMessage(ctx, message)
	case SDKClaude:
		return d.handleClaudeMessage(ctx, message)
	default:
		return fmt.Errorf("unknown sdk %q", d.sdk)
	}
}

func (d *Daemon) handleCodexMessage(ctx context.Context, message platform.Message) error {
	threadID := strings.TrimSpace(message.ThreadID)
	if threadID == "" {
		return fmt.Errorf("message %s missing thread id", message.ID)
	}
	inputText, err := buildInput(message)
	if err != nil {
		return err
	}
	codexThreadID, err := d.ensureCodexThread(ctx)
	if err != nil {
		return err
	}
	// Native mode does not route through the LLM Proxy, so there is nothing
	// here to wait for -- the vendor host is intercepted on dial.
	if !d.cfg.LLMNative {
		if err := waitForZitiLLMService(ctx, d.cfg.LLMBaseURL, llmReadyTimeout); err != nil {
			return fmt.Errorf("wait for LLM service before codex turn: %w", err)
		}
	}
	turnCtx, cancel := context.WithTimeout(ctx, turnStartTimeout)
	turnResp, err := d.codex.StartTurn(turnCtx, &codex.TurnStartParams{
		ThreadID: codexThreadID,
		Input:    []codex.UserInput{codex.NewTextUserInput(inputText)},
	})
	err = operationContextErr(turnCtx, err)
	cancel()
	if err != nil {
		return operationError(opCodexStartTurn, turnStartTimeout, fmt.Errorf("start codex turn for message %s on thread %s: %w", message.ID, codexThreadID, err))
	}
	turnID := strings.TrimSpace(turnResp.Turn.ID)
	if turnID == "" {
		return fmt.Errorf("codex turn id missing")
	}
	completionCh := d.tracker.Register(turnID)
	select {
	case result := <-completionCh:
		if result.ThreadID != "" && result.ThreadID != codexThreadID {
			return operationError(
				opCodexTurnResult,
				0,
				terminalCodexTurn(fmt.Errorf("codex turn %s thread mismatch for message %s", turnID, message.ID)),
			)
		}
		if result.Err != nil {
			if !errors.Is(result.Err, codexbridge.ErrMissingAgentMessage) {
				if isRetryableCodexErrorNotification(result.Err) {
					if codexWillRetryTurn(result.Err) {
						log.Printf("codex turn transient failure: turn_id=%s message_id=%s cause=%s; retrying sync", turnID, message.ID, result.Err)
						return operationError(
							opCodexTurnResult,
							0,
							fmt.Errorf("codex turn %s transient failure for message %s: %w", turnID, message.ID, result.Err),
						)
					}
					// codex is not retrying, so the turn is over, and running it
					// again only asks the same question for the same answer: a
					// response that completes carrying no assistant message is
					// reported this way and is an ordinary thing to return. End
					// it the way an empty turn ends.
					log.Printf("codex ended turn %s without a message for %s: %s", turnID, message.ID, result.Err)
					if err := d.publishFinalMessage(ctx, SDKCodex, message, ""); err != nil {
						return err
					}
					return d.ackMessage(ctx, message)
				}
				return operationError(
					opCodexTurnResult,
					0,
					terminalCodexTurn(fmt.Errorf("codex turn %s failed for message %s: %w", turnID, message.ID, result.Err)),
				)
			}
			readbackMessage, readbackErr := d.readCodexTurnMessage(ctx, codexThreadID, turnID)
			if readbackErr != nil {
				turnErr := fmt.Errorf("codex turn %s failed for message %s: %w; readback failed: %w", turnID, message.ID, result.Err, readbackErr)
				if errors.Is(readbackErr, codexbridge.ErrMissingAgentMessage) {
					turnErr = terminalCodexTurn(turnErr)
				}
				return operationError(
					opCodexTurnResult,
					0,
					turnErr,
				)
			}
			result.Message = readbackMessage
		}
		// A turn that ends without text is not a failure. An agent that
		// discards its final message says everything it has to say over the
		// CLI and ends silent by design, and one that posts it has nothing to
		// post -- publishFinalMessage handles both. The agn and claude paths
		// have always acked here; codex killing the daemon instead cost the
		// workload every thread it was serving.
		if strings.TrimSpace(result.Message) == "" {
			log.Printf("codex turn %s produced no text for message %s", turnID, message.ID)
		}
		if err := d.publishFinalMessage(ctx, SDKCodex, message, result.Message); err != nil {
			return err
		}
		if err := d.ackMessage(ctx, message); err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		d.tracker.Cancel(turnID)
		return operationError(
			opCodexWaitTurnCompletion,
			0,
			fmt.Errorf("wait for codex turn %s completion for message %s: %w", turnID, message.ID, ctx.Err()),
		)
	}
}

func (d *Daemon) readCodexTurnMessage(ctx context.Context, codexThreadID, turnID string) (string, error) {
	readbackCtx, cancel := context.WithTimeout(ctx, codexReadbackTimeout)
	defer cancel()

	ticker := time.NewTicker(codexReadbackPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		message, err := d.readCodexTurnMessageOnce(readbackCtx, codexThreadID, turnID)
		if err == nil {
			return message, nil
		}
		if !isRetryableCodexReadbackError(err) {
			return "", err
		}
		lastErr = err

		select {
		case <-readbackCtx.Done():
			return "", fmt.Errorf("read codex turn %s after completion timed out after %s: %w", turnID, codexReadbackTimeout, lastErr)
		case <-ticker.C:
		}
	}
}

func isRetryableCodexReadbackError(err error) bool {
	return errors.Is(err, codexbridge.ErrMissingAgentMessage)
}

func (d *Daemon) readCodexTurnMessageOnce(ctx context.Context, codexThreadID, turnID string) (string, error) {
	threadResp, err := d.codex.ReadThread(ctx, &codex.ThreadReadParams{
		ThreadID:     codexThreadID,
		IncludeTurns: true,
	})
	if err != nil {
		return "", fmt.Errorf("read codex thread %s: %w", codexThreadID, err)
	}
	for _, turn := range threadResp.Thread.Turns {
		if turn.ID != turnID {
			continue
		}
		message, err := codexbridge.ExtractFinalAnswer(turn)
		if err != nil {
			return "", err
		}
		return message, nil
	}
	return "", fmt.Errorf("codex turn %s not found in thread %s", turnID, codexThreadID)
}

// ensureCodexThread returns the codex thread this instance talks in, resuming
// it after a restart and starting one the first time.
//
// The instance, not the thread. Agent state is instance-scoped -- an instance
// serves many threads and they share one conversation -- and keying it by
// thread meant a reply arriving on another thread began a conversation with no
// memory of the one that started it: an agent that asked another agent a
// question could not read the answer in context. Which thread a message came
// from is in its header. The agn path was keyed this way in #161; this is the
// same change for codex.
func (d *Daemon) ensureCodexThread(ctx context.Context) (string, error) {
	if d.mappingStore == nil {
		return "", fmt.Errorf("codex mapping store is not configured")
	}
	instanceID := d.selfID()
	if record, ok := d.mapping.RecordForInstance(instanceID); ok {
		updated := record
		updated.LastUsedAtUnixMs = time.Now().UnixMilli()
		d.mapping.SetRecord(updated)
		if err := d.mappingStore.Save(updated); err != nil {
			return "", fmt.Errorf("save codex thread mapping for instance %s: %w", instanceID, err)
		}
		return record.CodexThreadID, nil
	}
	record, ok, err := d.mappingStore.Load(instanceID)
	if err != nil {
		return "", fmt.Errorf("load codex thread mapping for instance %s: %w", instanceID, err)
	}
	if ok {
		if err := d.resumeCodexThread(ctx, record.CodexThreadID); err != nil {
			return "", err
		}
		record.LastUsedAtUnixMs = time.Now().UnixMilli()
		d.mapping.SetRecord(record)
		if err := d.mappingStore.Save(record); err != nil {
			return "", fmt.Errorf("save codex thread mapping for instance %s: %w", instanceID, err)
		}
		return record.CodexThreadID, nil
	}
	codexThreadID, err := d.startCodexThread(ctx)
	if err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	newRecord := codexbridge.ThreadMappingRecord{
		InstanceID:       instanceID,
		CodexThreadID:    codexThreadID,
		CreatedAtUnixMs:  now,
		LastUsedAtUnixMs: now,
	}
	d.mapping.SetRecord(newRecord)
	if err := d.mappingStore.Save(newRecord); err != nil {
		return "", fmt.Errorf("save codex thread mapping for instance %s: %w", instanceID, err)
	}
	return codexThreadID, nil
}

func waitForZitiLLMService(ctx context.Context, llmBaseURL string, timeout time.Duration) error {
	addr, ok, err := zitiLLMServiceAddress(llmBaseURL)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return waitForTCPService(ctx, addr, timeout, "LLM service")
}

func zitiLLMServiceAddress(llmBaseURL string) (string, bool, error) {
	if !isZitiLLMBaseURL(llmBaseURL) {
		return "", false, nil
	}
	parsed, err := url.Parse(strings.TrimSpace(llmBaseURL))
	if err != nil {
		return "", false, fmt.Errorf("parse LLM base URL: %w", err)
	}
	host := parsed.Hostname()
	if host == "" {
		return "", false, fmt.Errorf("LLM base URL missing host")
	}
	port := parsed.Port()
	if port == "" {
		switch parsed.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", false, fmt.Errorf("LLM base URL scheme %q has no default port", parsed.Scheme)
		}
	}
	return net.JoinHostPort(host, port), true, nil
}

type tcpServiceTarget struct {
	label string
	addr  string
}

type mcpServiceTarget struct {
	label string
	addr  string
	url   string
}

func waitForTCPService(ctx context.Context, addr string, timeout time.Duration, label string) error {
	return waitForTCPServices(ctx, []tcpServiceTarget{{label: label, addr: addr}}, timeout)
}

func waitForTCPServices(ctx context.Context, targets []tcpServiceTarget, timeout time.Duration) error {
	if len(targets) == 0 {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, target := range targets {
		attempt := 0
		for {
			attempt++
			conn, err := net.DialTimeout("tcp", target.addr, 1*time.Second)
			if err == nil {
				_ = conn.Close()
				log.Printf("%s at %s ready after %d attempt(s)", target.label, target.addr, attempt)
				break
			}
			log.Printf("waiting for %s at %s (attempt %d): %v", target.label, target.addr, attempt, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return fmt.Errorf("%s at %s not ready after %s", target.label, target.addr, timeout)
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

func waitForMCPServers(ctx context.Context, servers []config.MCPServer, timeout time.Duration) error {
	targets := make([]mcpServiceTarget, 0, len(servers))
	for _, server := range servers {
		addr := net.JoinHostPort(mcpLoopbackHost, fmt.Sprintf("%d", server.Port))
		targets = append(targets, mcpServiceTarget{
			label: fmt.Sprintf("MCP server %s", server.Name),
			addr:  addr,
			url:   mcpEndpoint(server.Port),
		})
	}
	return waitForMCPServices(ctx, targets, timeout)
}

func mcpEndpoint(port int) string {
	return fmt.Sprintf("http://%s/mcp", net.JoinHostPort(mcpLoopbackHost, fmt.Sprintf("%d", port)))
}

func waitForMCPServices(ctx context.Context, targets []mcpServiceTarget, timeout time.Duration) error {
	if len(targets) == 0 {
		return nil
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	client := &http.Client{Timeout: 10 * time.Second}
	for _, target := range targets {
		attempt := 0
		for {
			attempt++
			if err := probeMCPService(ctx, client, target.url); err == nil {
				log.Printf("%s at %s ready after %d attempt(s)", target.label, target.addr, attempt)
				break
			} else {
				log.Printf("waiting for %s at %s (attempt %d): %v", target.label, target.addr, attempt, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return fmt.Errorf("%s at %s not ready after %s", target.label, target.addr, timeout)
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

func probeMCPService(ctx context.Context, client *http.Client, endpoint string) error {
	body := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agynd","version":"mcp-ready"}}}`, mcpProbeID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http status %s", resp.Status)
	}
	return readMCPProbeResult(resp.Body)
}

type mcpProbeResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

func readMCPProbeResult(body io.Reader) error {
	reader := bufio.NewReader(io.LimitReader(body, 64*1024))
	var raw bytes.Buffer
	var eventData []string
	var lastEventErr error
	sawSSE := false

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			raw.WriteString(line)
			trimmed := strings.TrimRight(line, "\r\n")
			if data, ok := strings.CutPrefix(trimmed, "data:"); ok {
				sawSSE = true
				eventData = append(eventData, strings.TrimSpace(data))
			} else if sawSSE && strings.TrimSpace(trimmed) == "" {
				if len(eventData) > 0 {
					if err := validateMCPProbePayload([]byte(strings.Join(eventData, "\n"))); err == nil {
						return nil
					} else {
						lastEventErr = err
					}
				}
				eventData = nil
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		return err
	}

	if sawSSE {
		if len(eventData) > 0 {
			if err := validateMCPProbePayload([]byte(strings.Join(eventData, "\n"))); err == nil {
				return nil
			} else {
				lastEventErr = err
			}
		}
		if lastEventErr != nil {
			return lastEventErr
		}
		return fmt.Errorf("initialize SSE response missing data")
	}
	return validateMCPProbePayload(raw.Bytes())
}

func validateMCPProbePayload(payload []byte) error {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return fmt.Errorf("initialize response is empty")
	}
	var response mcpProbeResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return fmt.Errorf("parse initialize response: %w", err)
	}
	if response.JSONRPC != "2.0" {
		return fmt.Errorf("initialize response jsonrpc %q does not match 2.0", response.JSONRPC)
	}
	if response.ID != mcpProbeID {
		return fmt.Errorf("initialize response id %q does not match %q", response.ID, mcpProbeID)
	}
	if len(bytes.TrimSpace(response.Error)) > 0 && !bytes.Equal(bytes.TrimSpace(response.Error), []byte("null")) {
		return fmt.Errorf("initialize response contains error")
	}
	result := bytes.TrimSpace(response.Result)
	if len(result) == 0 || bytes.Equal(result, []byte("null")) {
		return fmt.Errorf("initialize response missing result")
	}
	return nil
}

// codexModel picks what to pin the thread to: the platform Model UUID the
// proxy resolves, or the vendor's own model name. Unset in native mode leaves
// codex on its own default.
func codexModel(cfg config.Config, platformModel string) string {
	if cfg.LLMNative {
		return strings.TrimSpace(cfg.LLMModelName)
	}
	return strings.TrimSpace(platformModel)
}

type codexThreadDefaults struct {
	model            *string
	baseInstructions *string
	cwd              *string
}

func (d *Daemon) codexThreadDefaults() codexThreadDefaults {
	defaults := codexThreadDefaults{}
	if model := codexModel(d.cfg, d.agent.GetModel()); model != "" {
		defaults.model = &model
	}
	// The agent's system prompt is the system prompt: it is sent as the base
	// instructions, which codex renders in place of its own rather than
	// alongside them. An agent that needs the CLI's operating instructions has
	// to carry them in its own prompt. The role is an internal label and is
	// never shown to the model.
	if config, err := parseAgentConfiguration(d.agent.GetConfiguration()); err == nil {
		if prompt := strings.TrimSpace(config.SystemPrompt); prompt != "" {
			defaults.baseInstructions = &prompt
		}
	} else {
		log.Printf("agent configuration: %v; starting without a system prompt", err)
	}
	if d.cfg.WorkDir != "" {
		workDir := d.cfg.WorkDir
		defaults.cwd = &workDir
	}
	return defaults
}

func (d *Daemon) resumeCodexThread(ctx context.Context, codexThreadID string) error {
	params := &codex.ThreadResumeParams{ThreadID: codexThreadID}
	defaults := d.codexThreadDefaults()
	params.Model = defaults.model
	params.BaseInstructions = defaults.baseInstructions
	params.Cwd = defaults.cwd
	resp, err := d.codex.ResumeThread(ctx, params)
	if err != nil {
		return fmt.Errorf("resume codex thread: %w", err)
	}
	threadID := strings.TrimSpace(resp.Thread.ID)
	if threadID == "" {
		return fmt.Errorf("resume codex thread id missing")
	}
	if threadID != codexThreadID {
		return fmt.Errorf("resume codex thread id mismatch")
	}
	return nil
}

func (d *Daemon) startCodexThread(ctx context.Context) (string, error) {
	params := &codex.ThreadStartParams{}
	defaults := d.codexThreadDefaults()
	params.Model = defaults.model
	params.BaseInstructions = defaults.baseInstructions
	params.Cwd = defaults.cwd
	ephemeral := false
	params.Ephemeral = &ephemeral
	resp, err := d.codex.StartThread(ctx, params)
	if err != nil {
		return "", fmt.Errorf("start codex thread: %w", err)
	}
	return resp.Thread.ID, nil
}

func buildInput(message platform.Message) (string, error) {
	text := strings.TrimSpace(message.Body)
	if len(message.FileIDs) > 0 {
		lines := make([]string, 0, len(message.FileIDs)+1)
		if text != "" {
			lines = append(lines, text)
		}
		for _, fileID := range message.FileIDs {
			lines = append(lines, fmt.Sprintf("agyn://file/%s", fileID))
		}
		text = strings.Join(lines, "\n")
	}
	if text == "" {
		return "", fmt.Errorf("message %s has no content", message.ID)
	}
	return messageHeader(message) + text, nil
}

func messageHeader(message platform.Message) string {
	var header strings.Builder
	if threadID := strings.TrimSpace(message.ThreadID); threadID != "" {
		fmt.Fprintf(&header, "thread: %s\n", threadID)
	} else {
		header.WriteString("source: direct\n")
	}
	if handle := strings.TrimSpace(message.SenderHandle); handle != "" {
		fmt.Fprintf(&header, "from: @%s\n", handle)
	} else if senderID := strings.TrimSpace(message.SenderID); senderID != "" {
		fmt.Fprintf(&header, "from: %s\n", senderID)
	}
	header.WriteString("---\n")
	return header.String()
}

// recordInvocation exports the message that opened this turn. Tracing is an
// optional dependency, so a failure is reported and the turn proceeds -- an
// unreachable Tracing service must not cost the agent a message.
func (d *Daemon) recordInvocation(ctx context.Context, message platform.Message) {
	if d.tracing == nil {
		return
	}
	err := d.tracing.InvocationMessage(ctx, tracing.Message{
		ID:        message.ID,
		ThreadID:  message.ThreadID,
		SenderID:  message.SenderID,
		Body:      message.Body,
		CreatedAt: message.CreatedAt,
	})
	if err != nil {
		log.Printf("tracing: record invocation for message %s: %v", message.ID, err)
	}
}
