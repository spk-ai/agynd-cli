# Repository Guidance

## Living Documentation

- Keep non-obvious contracts and rationale beside the owning Go code; update
  comments and tests with behavior changes instead of narrating the implementation.
- Keep Markdown for setup, operations, security, cross-component coordination,
  and verification. Link to source instead of duplicating implemented behavior.
- Maintain `docs/catalog.json` as a curated Markdown index, not a generated
  component inventory. Preserve historical records and the existing `LICENSE`.

## Navigation

- When Navigator is available, first discover repositories with
  `repos --worktrees`, scan selected components with `scan --repo daemon`, then batch
  `inspect` related returned IDs before reading source. Follow symbols, comments,
  and test links with focused excerpts; use `docs --repo daemon` to select guides.
- Select alternate worktrees with `--worktree <Git-discovered basename>` and
  check returned provenance. Go methods use `Type.Method`. Cross-repo `@see`
  targets use repo selectors and extensionless components (for example
  `claude-sdk::options`); local targets retain their source extension and are
  repo-relative.
- Without Navigator, use native Go package structure and documentation, then
  targeted source/test reads. Do not require any particular workspace or lab path.

## Verification

- Use the Go version required by `go.mod` and existing generated API bindings
  under `.gen/`. Missing bindings are a prerequisite to report, not a reason for
  incidental generation or dependency changes in a documentation-only task.
- For behavior changes, select focused existing tests and run
  `go test -mod=readonly <packages>`; add `-race` for ownership and lifecycle work.
- Use a disposable `HOME` and an allowlisted environment without inherited
  CLI state, initialization, inbox, authentication, or live-fixture overrides.
  Native CLI, provider, cluster, and E2E fixtures need separate explicit
  authorization; never enable them for ordinary local verification.
- Run `gofmt` on edited Go files and `git diff --check`. For documentation-only
  work, compare source tokens and build/tool directives with the base; leave
  `go.mod`, `go.sum`, and generated output untouched. Report tests and skips.
