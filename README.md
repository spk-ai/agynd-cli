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
volume (for example `/agent-state/codex`). Codex configuration, authentication
state and native sessions then use that directory. Agyn's instance-to-Codex
session mappings are stored under `CODEX_HOME/agyn/thread-mapping` so a recreated
workload can resume the same native session. `HOME` and the workspace are unchanged.

Without an override, existing `HOME/.codex` state and
`HOME/.agyn/codex/thread-mapping` mappings retain their previous locations. Setting
`CODEX_HOME` to the default `HOME/.codex` also retains the legacy mapping path.
This setting does not migrate old state or create a persistent volume. To move an
existing instance, stop it first and migrate both its native state and its mapping;
copying only a mapping cannot restore a missing native session. Do not share a
state directory between instances, and treat it as private credential-bearing
storage, not a transcript export directory.

## Required Initialization

Set `AGYN_INIT_SCRIPTS_REQUIRED=true` in an operator-managed environment when
initialization is a prerequisite for agent execution. A nonzero environment or
agent init-script exit then aborts daemon setup before the agent CLI starts;
later scripts do not run. Invalid boolean values also abort setup. With the
variable unset or false, nonzero exits retain their existing log-and-continue
behavior. Context cancellation always aborts setup, in either mode.

This does not authenticate scripts or make agent execution exactly once. Use
trusted scripts, bounded startup checks and durable execution reconciliation.

## Durable inbox guard (opt-in)

Set `AGYN_INBOX_JOURNAL_DIR` to a private, daemon-owned directory on durable
instance storage to disable automatic replay of ambiguous agent turns. The
journal is shared by all SDK bridges; unset preserves the existing behavior.
It requires instance inbox items, not the legacy thread-ack path.

Intent is fsynced before invoking the agent. Completion is persisted before the
inbox ACK, so an ACK retry does not rerun the agent or republish its final reply.
A pending record after an agent error or process replacement stops processing
with a reconciliation error. It does not infer whether tools already succeeded.
Corrupt, missing-required, mismatched or unsafe state fails closed.

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

Only the allowed message may execute. Listed retired messages are durably
marked `ack_only` and acknowledged as the instance, without running an agent.
This is a discard decision, not proof that an interrupted turn completed. All
other inbox messages stop processing. Control requires the durable journal.

The coordinator must stop and verify removal of the previous workload before
authorizing a new one, and audit explicit reconciliation of ambiguous work.
Never build the control file from model output or silently retire an unknown
request. The journal stores identity and content hashes, not message bodies.
Keep it for the lifetime of the instance; deleting it removes replay protection.
External side effects still require their own idempotency or human review.
Neither a writable journal nor root-run agents provide a security boundary.

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
