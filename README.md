# agynd

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

## Reporting Init Image

Every push to `docs/living-contracts` publishes
`ghcr.io/spk-ai/agynd-cli-init:<commit>` from
[Dockerfile.reporting-init](Dockerfile.reporting-init) through the
[publish workflow](.github/workflows/publish-image.yml); pin the multi-platform
index digest that run reports in the orchestrator's `AGYND_CLI_INIT_IMAGE`.
Besides `agynd`, its trace hook, `tmux` and terminfo, the image delivers Node as
`/agyn/bin/node` (with its C++ runtime in `/agyn/lib`) for init scripts and
terminal commands such as execution reporting. The entrypoint runs as UID/GID
10001 and writes only `/agyn`.

The delivered Node is glibc-linked, so the workspace image must be glibc-based.
The execution reporting gate creates `/run/agyn-execution` as the workload user,
so a non-root workspace image must leave `/run` writable by that user. The
[init image check](test/initimage/initimage_test.go) owns the exercised
contract: a restricted workload, the pinned kind-a2a gate and receiver on a
TTY, and the agent CLI started after its MCP servers answer.

## Persistent Codex State

Provision a private durable volume per instance and configure `CODEX_HOME` for
that mount. Consult [env.go](internal/daemon/env.go) for state-root configuration
and [codexstate.go](internal/daemon/codexstate.go) for mapping placement before
moving existing data.

To move an existing instance, stop it first and migrate both its native state
and its mapping; copying only a mapping cannot
restore a missing native session. Do not share a state directory between
instances, and treat it as private credential-bearing
storage, not a transcript export directory.

## Required Initialization

Enable `AGYN_INIT_SCRIPTS_REQUIRED=true` when initialization is a deployment
prerequisite; the configuration contract lives in
[init_scripts.go](internal/daemon/init_scripts.go).

This does not authenticate scripts or make agent execution exactly once. Use
trusted scripts, bounded startup checks and durable execution reconciliation.

## Durable inbox guard (opt-in)

Provision `AGYN_INBOX_JOURNAL_DIR` on private, daemon-owned durable instance
storage. For coordinator authorization, prepare `AGYN_INBOX_CONTROL_FILE`
according to [inboxjournal.Control](internal/inboxjournal/journal.go).
Configuration is owned by [inbox_journal.go](internal/daemon/inbox_journal.go).

The coordinator must stop and verify removal of the previous workload before
authorizing a new one, and audit explicit reconciliation of ambiguous work.
Never build the control file from model output or silently retire an unknown
request. Retiring a message is a discard decision, not proof of completion.
Keep the journal for the instance lifetime; deleting it removes replay protection.
External side effects still require their own idempotency or human review.
Neither a writable journal nor root-run agents provide a security boundary.

## Persistent Claude Sessions (Opt-In)

Provision one private durable workspace per managed instance. Configure native
state and separate session metadata roots using
[claudesession.go](internal/daemon/claudesession.go); consult the binding contract
in [claudebridge](internal/claudebridge/session.go) before migrating an instance.
Mount the parent workspace rather than precreating the session-metadata leaf.

This is completed-turn continuity, not permission to replay interrupted work.
The execution coordinator must reconcile possible side effects and fence old
workloads before allowing another turn. The advisory filesystem lock requires
working local filesystem locking and is not a security boundary against the
agent, escaped children or a partitioned node. Do not treat session persistence
as a tool-permission boundary.

Use [AGENTS.md](AGENTS.md) for credential-free verification discipline. Native
CLI and Agyn/Pod acceptance remain separate verification requirements.

## E2E validation

See [claude.go](internal/daemon/claude.go) for failure handling. Reconcile possible
side effects before retrying; an error does not prove no tools ran.

The session-selection and diagnostic patches remain separate upstream review
units. This checkout combines them for local acceptance, not as a bundled
upstream proposal. Replace the SDK fork pin in `go.mod` with an upstream release
before production use or upstream merge.

Run the [native diagnostic fixture](internal/daemon/claude_diagnostic_live_test.go)
only with an explicitly selected CLI and separate authorization. Use a
network-denied environment to independently exclude external traffic; endpoint
redirection alone is not network isolation. Its compatibility evidence does not
explain historical provider failures or establish production authentication
reliability. The fixture source owns its opt-in inputs and assertions.

Use the [E2E workflow](.github/workflows/e2e.yml) for executable prerequisites
and the repo-local harness invocation; do not reuse production credentials for
local fixtures.

This repository does not run the centralized `agynio/e2e` smoke suite. Broader
platform and service smoke coverage remains owned by the centralized E2E
repository and service-specific workflows.
