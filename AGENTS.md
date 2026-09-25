# Repository Guidance

## Living Documentation

- Keep implemented contracts beside their owning Go package, type, or function.
  Document state ownership, persistence, ordering, failure/retry policy, and
  compatibility limits; update those comments with the code and its tests.
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
- For standalone upstream use without Navigator, fall back to native Go package
  structure, `git diff upstream/main --stat`, `rg --files`, `go list ./...`,
  `go doc`, and targeted source/test reads. Do not require Navigator or any
  particular workspace or absolute lab path.

## Verification

- Use the Go version required by `go.mod` and existing generated API bindings
  under `.gen/`. Missing bindings are a prerequisite to report, not a reason for
  incidental generation or dependency changes in a documentation-only task.
- Run focused existing tests for touched owners, for example
  `go test -mod=readonly ./internal/claudebridge ./internal/inboxjournal ./internal/codexbridge ./internal/daemon`;
  use `-race` for ownership, workers, and replay-sensitive changes.
- Use a disposable `HOME` and clear inherited `CODEX_HOME`, `CLAUDE_CONFIG_DIR`,
  `AGYN_CLAUDE_SESSION_DIR`, `AGYN_INBOX_JOURNAL_DIR`, `AGYN_INBOX_CONTROL_FILE`,
  and `AGYN_INIT_SCRIPTS_REQUIRED`. Constructors write first-run state.
- Leave `AGYN_NATIVE_DNS_TEST` and `AGYN_CLAUDE_DIAGNOSTIC_TEST` unset. Native CLI,
  provider, cluster, credential, and `-tags e2e` fixtures need separate explicit
  authorization; never enable them for ordinary local verification.
- Run `gofmt` on edited Go files and `git diff --check`. For documentation-only
  work, verify source tokens/AST are unchanged apart from comments; leave
  `go.mod`, `go.sum`, and generated output untouched. Report tests and skips.
