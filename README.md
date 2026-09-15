# agynd

The agynd daemon bridges agent CLIs with the platform by connecting to Threads
and Notifications services, preparing the agent runtime environment, and managing
the agent process lifecycle.

Architecture: https://github.com/agynio/architecture/blob/main/architecture/agynd-cli.md

## Shell Title Worker Lifetime

The focused `fix/shell-title-worker-lifetime` contribution is based on upstream
`495920a`. Shell startup returns an idempotent cancel-and-wait cleanup for its
title refresher, and `Daemon.Run` defers that cleanup. It does not terminate tmux
or its persistent sessions. Missing binaries and failed startup remain nonfatal.

The unchanged repeated shell test reproduced races between the background worker
and restored test paths. Tests now join the worker before restoring dependencies
and check explicit stop, parent cancellation, startup failure and an in-flight
refresh that must finish before cleanup returns. No timing globals are changed.

On 2026-09-15, 100 repeated focused race entries and all 383 upstream full race
entries pass with no failures or skips. Build and unfiltered vet also pass:

```sh
env -u CODEX_HOME go test -race ./internal/daemon \
  -run 'Test(StartShellServer|ShellServerEnv)' -count=10 -timeout=2m
env -u CODEX_HOME go test -race ./... -count=1 -timeout=3m
go build ./...
go vet ./...
```

Use the upstream generated API set. No API, provider, session-storage or runtime
image change is part of this patch. Source acceptance does not claim a deployed
image upgrade or overall production readiness; repository licensing is unchanged.

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
