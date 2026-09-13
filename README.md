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

## Persistent Claude Sessions (Opt-In)

For a managed Claude instance with a durable `/workspace`, configure:

```text
CLAUDE_CONFIG_DIR=/workspace/.claude
AGYN_CLAUDE_SESSION_DIR=/workspace/.agyn/claude-session
```

These must be separate, private, absolute paths on the instance's own durable
filesystem. Keep the agent ID, instance ID and working directory unchanged on
replacement. The daemon creates both directories with mode `0700`; do not
precreate the `claude-session` leaf or reuse it for another instance. Mount the
parent workspace, not that leaf. Existing native history without a mapping
requires explicit migration and is not adopted automatically.

Before starting the CLI, the daemon durably reserves a UUID and binds it to the
agent, instance, workspace and native state location. It holds an exclusive
nonblocking filesystem lock while it owns that session. On replacement it
verifies the binding and the native transcript's session/workspace metadata,
then passes the exact transcript path to the SDK's `Resume` option. A new
session uses `SessionID`. Settings, user state and skills honor
`CLAUDE_CONFIG_DIR`, so an ephemeral `HOME` does not change their location.

Missing, corrupt, ambiguous or mismatched state stops startup; it never silently
creates a replacement conversation. This includes a reserved UUID with no
native transcript after an interrupted first startup. A mismatched turn result
stops processing before publishing the reply or acknowledging the inbox.
Malformed persistent user state is preserved for reconciliation instead of
being reset. Holder mode cannot use this option. With the session-directory
variable unset, existing session-selection behavior is unchanged.

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

Run daemon tests with an isolated `HOME` and without inherited `CODEX_HOME`,
`CLAUDE_CONFIG_DIR` or `AGYN_CLAUDE_SESSION_DIR`; existing constructor tests write
first-run CLI state. Native CLI and Agyn/Pod acceptance are separate from these
credential-free tests.

## E2E validation

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
