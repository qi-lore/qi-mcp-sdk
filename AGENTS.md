# AGENTS.md

Independent Go SDK for the Model Context Protocol. **Implementation has begun**: `go.mod`,
`go.work`, the six root package directories, `.golangci.yml`, and the `.github/workflows/`
definitions (`ci.yml`, `commit-conventions.yml`) all exist, so `go build ./...` and `go vet ./...`
compile real packages instead of printing `"./..." matched no packages`. Progress lives only in
the checkboxes of `specs/001-go-mcp-sdk/tasks.md`.

**Picking up interrupted work?** Jump to [Resuming an interrupted task](#resuming-an-interrupted-task) —
a fresh session has no memory of what the previous one did, and this repo hides progress from git.

## Source of truth

Implementation is under way. The design lives in a place `git status` will not show you:

- `specs/001-go-mcp-sdk/` — `spec.md` (58 FR / 11 NFR / 15 SC), `plan.md`, `research.md` (R-001…R-019),
  `data-model.md`, `contracts/` (wire, core-api, transport, cli), `quickstart.md`, and
  `tasks.md` (**125 tasks; its checkboxes are the work queue and the only progress record**).
- `.specify/memory/constitution.md` — binding: 11 principles, merge gates, and the task
  iteration protocol. Read it before implementing anything.
- `docs/decisions/0001-protocol-eras.md` — the FR-056 era decision record.

Read the design artifacts before writing code. `tasks.md` task descriptions quote the exact
constraints (validation rules V1–V20, 4 MiB default bound, algorithm allowlist, etc.).

## Resuming an interrupted task

A previous session may have been cut off mid-task. You have no memory of it; the repo does.
This is the procedure for picking it up — follow the steps, do not guess.

**1. Find the task.** Progress is tracked in exactly one place: the `- [ ]` / `- [x]` checkboxes in
`specs/001-go-mcp-sdk/tasks.md`. Nothing else records it. The task to resume is the first unchecked
one in dependency order (Phase 1 → 9, ascending id within a phase). Derive it every time:

```bash
grep -m1 '^- \[ \]' specs/001-go-mcp-sdk/tasks.md    # the next task
grep -c '^- \[ \]'  specs/001-go-mcp-sdk/tasks.md    # how many remain
```

**Order exceptions.** A task line may carry an `**Order exception (user-approved, …)**` note that
overrides the ascending-id rule — the `grep` above still returns that task, so read the line before
starting it. Ordering exceptions are recorded where they belong, in `tasks.md` under *Dependencies &
Execution Order*, and repeated on the affected task line so a mechanical pick cannot miss it. Do not
act on a task whose line says to run a different one first.

**2. Assume it was interrupted, not finished.** A task is checked only *after its tests pass*, so an
unchecked task may already have partial or complete work in the tree. Conversely, never assume a
checked task is trustworthy — verify before building on it.

**3. Look for the previous session's work.** Code is visible to git; the design artifacts are not:

```bash
git status --short && git diff            # code changes, tracked and untracked
find specs docs .specify -type f -newer .git/HEAD   # design files edited since the last commit
```

**Danger:** `specs/`, `.specify/`, `.opencode/`, and `docs/` are gitignored, so git reports
**nothing** for edits inside them. A previous agent may have rewritten `plan.md`, `research.md`, or
`tasks.md` and `git status` will look clean. The `find -newer .git/HEAD` probe is the only way to
see it — right now it correctly reports `tasks.md`, `plan.md`, and `constitution.md` as post-commit
edits.

**4. Check the task's own files.** Each task description names exact paths (e.g. `core/codec.go`,
`core/codec_test.go`). If they already exist, the task was at least partly done.

**5. Do not restart from scratch.** If the task's files exist, run its test **before** changing
anything:

```bash
go test ./core -run TestDecode -race
```

Passing tests mean the task is effectively complete — report that and ask whether to check the box.
Only rewrite what actually fails. Recreating files that are already correct is the most common way
to destroy the previous session's work.

**6. Re-read the cited rule before writing.** Every task text quotes the contract or data-model rule
it enforces. Open that section rather than inferring the shape — and for the three *(provisional)*
wire constants, confirm the value is verified (tasks T007–T009) before using it.

**7. Report as a resumption, then stop.** State: which task, what already existed before you
touched it, what you changed, what you verified, and anything still open. Then wait for the
explicit `next task` / `rework` instruction.

**Checkbox discipline.** Flip `[ ]` → `[x]` one task at a time, only after that task's tests pass.
Never check a task you did not verify, and never batch-check a group.

**Known gap.** The task report SC-010 requires has no repo-side home while the artifacts are
untracked, and nothing persists it locally. If you find no trace of a previous report, you cannot
recover what that agent concluded — re-derive it from the tree using step 3 and say so explicitly
in your report. Do not create a new tracked state file to work around this; the untracked-artifact
design is deliberate (see Gotcha 1).

## Gotcha 1 — `specs/`, `.specify/`, `.opencode/`, `docs/` are gitignored on purpose

`.gitignore` lines 33–36. This is deliberate and load-bearing:

- The mechanism is *untracked ⇒ absent from `git archive` ⇒ absent from the published module zip*
  (FR-033, NFR-008, SC-006). Nothing else is needed.
- **Do not** `git add -f` them, and do not "fix" this by tracking them.
- **Do not** reintroduce `.gitattributes export-ignore` — it does not work: `cmd/go` appends
  `* -export-subst -export-ignore` to `$GIT_DIR/info/attributes` before running `git archive`,
  overriding tracked `.gitattributes`. **Do not** add nested marker `go.mod` files to artifact
  trees either. Both were implemented, measured against the real toolchain, and rejected; the
  evidence is in `research.md` R-002. Re-proposing either is a regression.
- Consequence: no spec artifact is reviewable as a diff. Traceability runs through task reports
  and PR descriptions instead (constitution 0.2.0).

## Gotcha 2 — the iteration protocol (expect this to differ from normal agent behaviour)

Per `constitution.md` "Task Iteration Protocol" and FR-045–FR-050:

1. Complete **exactly one** task from `tasks.md`, then stop.
2. Report: task id and outcome, what changed and why, checks run with results, deviations/risks.
   No boilerplate — restating the task description is not a report. Unless a task is scoped to
   docs only, the checks that were run MUST include `go build ./...`, `go vet ./...`,
   `go test -race ./...`, and `golangci-lint run ./...` (v2 — see "Local toolchain paths").
3. **Wait** for an explicit instruction: `rework`, `create pull request`, or `next task`.
4. Never begin the next task, open a PR, or merge on your own initiative.
5. A task that cannot be finished is reported **blocked** with the concrete blocker. Never
   substitute scope.

**Resuming a task the previous session left unfinished?** The mirror image of this protocol is in
[Resuming an interrupted task](#resuming-an-interrupted-task) above — read it before touching
anything.

## Gotcha 3 — the root module must keep zero `require` directives

- Root `go.mod` (`github.com/qi-lore/qi-mcp-sdk`) has no `require` and must stay that way.
  Verified: `go list -m all` in a consumer importing only `core` lists *every* module the root
  requires, even unused ones — so one third-party `require` breaks SC-002/SC-003/SC-009.
- Anything needing a dependency goes in its own nested module: `adapters/grpc`, `adapters/nats`,
  `adapters/officialsdk`, `config` (YAML), `cmd` (CLI). `gopkg.in/yaml.v3` is the only new dep.
- Run dependency checks with **`GOWORK=off`** — a `go.work` would mask exactly the failure they
  exist to catch.
- `go.work` must name modules explicitly; never `go work use ./...`.

## Local toolchain paths (binding)

- **Never call, read, or resolve anything under `/opt/homebrew/bin`** — not even a version probe.
  That Homebrew prefix is not ours.
- Our Homebrew is `/Users/aiagent/homebrew/bin/brew`. Install and look up tools there.
- `golangci-lint` at `/Users/aiagent/go/bin/golangci-lint` is **v1.64.8 and must not be used**: it
  cannot type-check a package that imports the standard library under the go1.27 toolchain
  (`export data version 5 is greater than maximum supported version 2`), and it reports exit 0 for
  packages that import nothing, so its "0 issues" is not evidence of linting.
- The **v2** binary is now installed in our prefix and is the one on `PATH`:
  `/Users/aiagent/homebrew/bin/golangci-lint` → `Cellar/golangci-lint/2.15.0/bin/golangci-lint`
  (`2.15.0`, built with go1.27.2). It is the only sanctioned linter and honors `.golangci.yml`.
  Verified 2026-11-20: it flags planted `errcheck`/`gofmt` violations, so a green run from it is
  real evidence of linting.
- **Never use `curl`.** Use `my_curl`, the alias for `/Users/aiagent/homebrew/opt/curl/bin/curl`.
  Aliases do not exist in non-interactive shells, so invoke that absolute path directly.

## Commands

```bash
go build ./...            # compiles the six root packages today
go vet ./...
go test -race ./...       # required by NFR-006
golangci-lint run ./...   # v2 ONLY; part of every task's verification (see "Local toolchain paths")
go test ./core -run TestDecode -race        # single package / single test
GOWORK=off go list -m all                   # inside a temp consumer module: dependency leakage
```

Lint is configured in `.golangci.yml` — golangci-lint **v2 schema**: `govet`, `staticcheck`,
`errcheck` as linters; `gofmt`, `goimports` as formatters; `default: none` so the set cannot drift
when upstream defaults change. `.github/workflows/ci.yml` runs it pinned to a v2 release.

```bash
golangci-lint run ./...     # v2 ONLY. v1.64.8 prints "0 issues" for real code and exits 0.
golangci-lint cache clean   # stale cache can hide a violation; clean before trusting a run
```

Verified against golangci-lint 2.15.0, the binary installed in our Homebrew prefix and on `PATH`.
The `/Users/aiagent/go/bin/golangci-lint` v1 binary must not be used — see "Local toolchain paths".

Artifact-scope check (SC-006), reproducible without tooling:

```bash
git ls-tree -r --name-only <rev> -- specs .specify .opencode docs   # expect: no output
git archive --format=tar <rev> | tar -tf - | grep -E '^(specs|\.specify|\.opencode|docs)/'   # expect: no match
```

Everything outside the four untracked artifact directories is tracked. The empty directories
(`internal/`, `adapters/*`, `config/`, `cmd/`) are not recordable by git until their first file
lands (T070, T073, T077, T081, T108, T112) — a fresh clone will not contain them yet.

## Conventions that differ from defaults

- **Commits: Conventional Commits AND single-line only.** Multi-line commit bodies are explicitly
  forbidden (Principle IX). A one-line subject is the whole message. `.github/workflows/commit-conventions.yml`
  enforces this on every pull request, and fails any feature branch not targeting `dev` or the
  integration branch not targeting `main`.
- Branches: feature branches from `main`; feature PRs target `dev`; only `dev → main` merges to
  `main`. `main` and `dev` both exist locally; the feature branch is `001-go-mcp-sdk`.
- **No `origin` remote is configured.** Only the Gitt agent may push. Prepare a push-ready commit
  and ask the user to invoke it.
- **Tests are mandatory and test-first** (Principle II, FR-036) — write the failing test, confirm
  it fails for the expected reason, then implement. Tests are not optional here.
- **Never write diagnostics to stdout.** Under stdio, stdout carries the protocol. All diagnostics
  go through the injected `core.Logger`, silent by default (FR-057, NFR-011).
- Every blocking public API takes `context.Context` first; errors wrap with `%w` and are
  inspectable with `errors.Is`/`errors.As` (NFR-004, NFR-005).

## Protocol invariants an agent will get wrong

- Modern era `2026-07-28` is primary **and the default**: per-request `_meta`, `server/discover`,
  and **no** `initialize` handshake and **no** session identifier.
- Legacy is **`2025-11-25` only**, enabled explicitly via `core.Era`, and **never inferred from
  observed traffic**. Any other handshake revision gets a typed negotiation error.
- Three wire constants are marked *(provisional)* and must be verified against the authoritative
  MCP `2026-07-28` revision **before** writing the code that depends on them (tasks T007–T009):
  the JSON-RPC codes `-32001`/`-32002`, the `server/discover` member names, and HTTP `404` for
  method-not-found. The typed errors in `core` — not the numeric codes — are the stable contract.
  Do not invent protocol behaviour (Principle III).

## Open loose ends

- `.pnpm-debug.log` sits in a Go repo. It is ignored now; still on disk. Harmless.
- SC-010 ("task reports visible in the issue or PR thread") has no repo-side home while the
  artifacts are untracked.

## SpecKit

The workflow is `constitution → specify → clarify → plan → checklist → tasks → analyze → implement`,
driven by `.opencode/commands/speckit.*` (e.g. `/speckit.implement`, `/speckit.analyze`). The active
feature is registered in `.specify/feature.json`. `.specify/extensions.yml` does not exist, so there
are no hooks to dispatch.
