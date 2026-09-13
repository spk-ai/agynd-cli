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

## E2E validation

Claude SDK calls can return a result with `IsError` set and no Go error (for
example, a native API authentication failure). The daemon treats error or nil
results as terminal processing failures: it neither publishes a final reply nor
acknowledges the inbox, and its sync loop does not automatically retry the turn.
The upstream error body is not included in the processing error. Reconcile
possible side effects before retrying; an error does not prove no tools ran.

Failed-result errors now include allowlisted result subtype/terminal reason and
an HTTP error status from 400 through 599 when reported by the CLI. Unknown or
unsafe strings become `unknown`; absent/invalid status becomes `0`. Response
bodies, arbitrary stop reasons and native session identifiers are not included.
`IsError` remains authoritative even when the result subtype is `success`.

This diagnostic change is stacked on `fix/claude-error-results` and temporarily
pins [the independent SDK metadata patch](https://github.com/spk-ai/claude-sdk-go/tree/feat/result-diagnostics).
Replace the fork pin with an upstream SDK release before merge. It contains no
A2A state machine, automatic retry policy, session-persistence or Kubernetes
integration. After the normal API generation, focused verification is
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
