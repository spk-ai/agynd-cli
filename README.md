# agynd

The agynd daemon bridges agent CLIs with the platform by connecting to Threads
and Notifications services, preparing the agent runtime environment, and managing
the agent process lifecycle.

Architecture: https://github.com/agynio/architecture/blob/main/architecture/agynd-cli.md

## Local Development

Full setup: https://github.com/agynio/architecture/blob/main/architecture/operations/local-development.md

### Run from sources

`agynd` is not deployed as a service. The [Runner](https://github.com/agynio/architecture/blob/main/architecture/agynd-cli.md)
starts it as the main process of an agent container, so there is no Deployment
to attach to and no `devspace dev` to run -- the way to exercise a change is to
put the binary in an agent init image and point an agent at it:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .build/agynd ./cmd/agynd
```

Then build an init image that copies `.build/agynd` over the one baked in, load
it into the local VM, and set it as the agent's init image:

```bash
agyn local load-image my-agent-init:dev
```

## Persistent Codex State

Set `CODEX_HOME` to an absolute path on an environment's per-instance persistent
volume (for example `/agent-state/codex`). State-root and legacy mapping-path
contracts live in [env.go](internal/daemon/env.go) and
[codexstate.go](internal/daemon/codexstate.go).

Provision storage separately. To move an existing instance, stop it first and
migrate both its native state and its mapping; copying only a mapping cannot
restore a missing native session. Do not share a state directory between
instances, and treat it as private credential-bearing
storage, not a transcript export directory.

## Required Initialization

Set `AGYN_INIT_SCRIPTS_REQUIRED=true` in an operator-managed environment when
initialization is a prerequisite for agent execution. Ordering, boolean parsing,
and failure policy live in [init_scripts.go](internal/daemon/init_scripts.go).

This does not authenticate scripts or make agent execution exactly once. Use
trusted scripts, bounded startup checks and durable execution reconciliation.

## Durable inbox guard (opt-in)

Set `AGYN_INBOX_JOURNAL_DIR` to a private, daemon-owned directory on durable
instance storage to guard against replay of ambiguous agent turns. The record
and coordinator-control contracts live in
[inboxjournal](internal/inboxjournal/journal.go); SDK dispatch and ACK ordering
are owned by [daemon.go](internal/daemon/daemon.go).

A coordinator can also set `AGYN_INBOX_CONTROL_FILE` to an absolute path to a
private JSON file installed before inbox consumption:

```json
{
  "version": 1,
  "instance_id": "1c2f0f4e-8b9d-4a5b-9b3c-1d2e3f4a5b6c",
  "allowed_message_id": "new-message-id",
  "ack_only_message_ids": ["explicitly-retired-message-id"]
}
```

The coordinator must stop and verify removal of the previous workload before
authorizing a new one, and audit explicit reconciliation of ambiguous work.
Never build the control file from model output or silently retire an unknown
request. Retiring a message is a discard decision, not proof of completion.
Keep the journal for the instance lifetime; deleting it removes replay protection.
External side effects still require their own idempotency or human review.
Neither a writable journal nor root-run agents provide a security boundary.

## Persistent Claude Sessions (Opt-In)

For a managed Claude instance with a durable `/workspace`, configure:

```text
CLAUDE_CONFIG_DIR=/workspace/.claude
AGYN_CLAUDE_SESSION_DIR=/workspace/.agyn/claude-session
```

These must be separate, private, absolute paths on the instance's own durable
filesystem. Keep the agent ID, instance ID and working directory unchanged on
replacement. Do not precreate the `claude-session` leaf or reuse it for another
instance. Mount the parent workspace, not that leaf. Existing native history
without a mapping requires explicit migration and is not adopted automatically.

Binding, lock ownership, and transcript validation are documented in
[claudebridge](internal/claudebridge/session.go). Environment selection lives in
[claudesession.go](internal/daemon/claudesession.go), user-state preservation in
[claudestate.go](internal/daemon/claudestate.go), and SDK selection/result checks
in [claude.go](internal/daemon/claude.go).

This is completed-turn continuity, not permission to replay interrupted work.
The execution coordinator must reconcile possible side effects and fence old
workloads before allowing another turn. The advisory filesystem lock requires
working local filesystem locking and is not a security boundary against the
agent, escaped children or a partitioned node. CLI tool-approval behavior is
unchanged. The focused branch temporarily pins the reviewed SDK session-option
fork in `go.mod`; replace it with an upstream release before upstream merge.

Focused credential-free tests cover allocation/resume, process-lock recovery,
concurrent owners, damaged state, relocated configuration, terminal session
mismatches and shutdown ordering:

```bash
go test ./internal/claudebridge ./internal/daemon
go test -race ./internal/claudebridge ./internal/daemon -run 'Session|Claude|Skills'
```

Run daemon tests with an isolated `HOME` and no inherited state, journal, init,
or live-test overrides; see [AGENTS.md](AGENTS.md) for local test discipline.
Existing constructor tests write first-run CLI state. Native CLI and Agyn/Pod
acceptance are separate from these credential-free tests.

## E2E validation

Claude failure handling and safe diagnostic fields are documented beside
`handleClaudeMessage` and `claudeTurnFailure` in
[claude.go](internal/daemon/claude.go). Reconcile possible side effects before
retrying; an error does not prove no tools ran.

The independently reviewable diagnostic change is stacked on
`fix/claude-error-results`. This `lab/claude-diagnostics-integration` branch
combines it with session persistence, the inbox guard and required init scripts
for local acceptance only. It pins the SDK's lab session/diagnostics combination;
replace that fork pin with an upstream SDK release before production use.
Neither combined branch is a bundled upstream proposal. No A2A state machine or
Kubernetes integration is added to the daemon. After normal API generation,
focused verification is
`go test -race ./internal/daemon -run 'ClaudeError' -count=1` with an isolated HOME.
These tests verify safe diagnostics and no reply/ACK after failure, not the cause
of a past native failure or production authentication reliability.

`TestClaudeDiagnosticNative401` is opt-in through
`AGYN_CLAUDE_DIAGNOSTIC_TEST=true` and an absolute
`AGYN_CLAUDE_DIAGNOSTIC_BINARY`. It uses fresh configuration/workspace directories,
replaces provider credentials with a nonsecret fixture value, and points the CLI
at a loopback server that always returns HTTP 401. It verifies the actual SDK
result reaches the daemon as a terminal failure with safe status metadata and
no reply or inbox ACK. The fixture drains the bounded request body and marks the
response non-retryable with `x-should-retry: false`. Native tools and auto-update
are disabled; first-run state uses the daemon's existing initializer. No model
backend serves the request. Run it in a
network-denied environment to independently exclude external traffic; ordinary
tests skip it. This does not reproduce or explain a historical provider failure.

The GitHub E2E workflow runs this repository's local E2E tests with:

```bash
go test -v -count=1 -tags e2e ./test/e2e/
```

Those tests validate the local agent CLI bridge behavior against deterministic
TestLLM endpoints through the Codex and AGN SDK flows. The workflow checks out
`agynio/agn-cli` so the AGN coverage builds and runs the current AGN CLI during
the test. They also build and execute this repository's `cmd/agynd` binary with
`/agyn/config.json` installed by the test harness. That test uses a stub
Gateway and fake AGN agent to verify daemon startup, platform initialization,
and subscriber startup without requiring a full cluster.

This repository does not run the centralized `agynio/e2e` smoke suite. Broader
platform and service smoke coverage remains owned by the centralized E2E
repository and service-specific workflows.
