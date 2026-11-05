# fixlab

Measures how accurately a candidate change implements a task: a branch, a
patch, or the work of an AI coding agent. Each run checks out the task's
`base_commit`, applies the candidate, adds hidden tests the candidate never
saw, runs the project's static checks and test suite, then load-tests the
candidate against an unchanged baseline and compares p95 latency and 5xx rate.
Every run writes one flat JSON record, and can be recorded in a history repo
so pass rates can be compared across models, prompts and CLAUDE.md versions.

```
go install github.com/luizeduardocarvalho/fixlab/cmd/fixlab@latest

cd my-workspace
fixlab run -task tasks/issue-9-x/task.yaml -runs 3
fixlab run -task … -candidate-ref my-branch          # evaluate a branch or commit
fixlab run -task … -candidate-patch change.diff      # evaluate a patch file
fixlab clean                                         # remove anything a killed run left behind
```

With no candidate, the task's `reference_commit` (the known-good fix) is
evaluated, which is how a new task is validated. Exit status is 0 only if
every run passed.

fixlab knows nothing about a project beyond two things the project provides:

- **A profile**, `.eval/profile.yaml` at the project's repo root: how to build
  and test it, which services its stack needs, seed data, the load script and
  what an agent may run. See [Project profile](#project-profile). The same
  profile runs the project's stack for direct use, by a person or an agent
  reproducing a bug:

  ```
  fixlab env up    -project ../my-api   # build the app from the working tree, start its stack, seed it
  fixlab env down  -project ../my-api
  fixlab env check -project ../my-api   # prove the profile works: stages pass, stack seeds, load runs clean
  ```

- **A workspace**: a directory holding the project's `tasks/`, and, for agent
  runs, `prompts/`, `strategies/`, `benchmark.yaml` and any CLAUDE.md variants.
  Every command but `env` runs in one (`-workspace`, default the current dir).
  It can be a directory of the project's repo or a repo of its own; when it is
  inside the project's repo, the repo is found from it (`-api-repo` otherwise).

`testdata/example/` is a small, made-up workspace and profile showing every
file's shape. Two skills for Claude Code walk an agent through the work:
`eval-profile` (`skills/eval-profile/`) writes a profile for a new project, and
`eval-task` (`skills/eval-task/`) takes an issue from report to merged fix:
spec, hidden tests, reference search, promotion, controls, validation and PR.
Install one by linking its directory into your skills dir, e.g.
`ln -s "$PWD/skills/eval-task" ~/.claude/skills/eval-task`.

## Requirements

- Docker (Desktop or Engine) with Compose v2, running. Projects whose tests use
  testcontainers skip them without Docker, **and fixlab counts a skipped hidden
  test as not passed**.
- Go 1.25+ on the host (`-go /path/to/go` if it is not on `PATH`): fixlab is
  built with it, and Go projects' stages use it.
- Network on first use, to pull pinned images and the tools the profile's
  stages fetch.
- The [Claude Code](https://claude.com/claude-code) CLI, for agent runs.
- `pnpm` only for `repo: web` tasks.

## What a run does

```
git worktree add base   (base_commit, untouched)          ┐ both under <workspace>/.work/<run-id>/
git worktree add cand   (base_commit + candidate + hidden) ┘
  │
  ├─ apply        Implementer.Implement(cand); copy hidden tests over
  ├─ stages       the profile's stages, in order, for example:
  │   ├─ build    go build ./...      (gate: a failure skips everything after it)
  │   ├─ vet      go vet ./...        (type-checks test files, hidden ones included)
  │   ├─ lint     staticcheck: findings in cand not in base  (pre-existing ones ignored)
  │   └─ go_test  go test -json on cand, once per package with tests
  ├─ hidden_tests every top-level Test in the hidden files passed (skip ≠ pass)
  ├─ stack_up     compose up base + cand stacks in parallel, rendered from the profile
  ├─ seed         same synthetic data into both
  ├─ load_test    same k6 script against each, in interleaved rounds (A B B A A B),
  │               one k6 at a time per host
  ├─ perf         cand vs base: median p95 over rounds, 5xx rate, expected-status rate
  └─ teardown     compose down -v --rmi local, git worktree remove   (always: failure, Ctrl-C, panic)
```

`repo: web` tasks have no profile yet and run built-in stages for a pnpm +
Vite app in the repo given by `-web-repo`: `pnpm install`, `vite build`,
`eslint`, `tsc -p tsconfig.app.json` (as a delta against base) and Vitest.
Go and perf stages are `skipped`.

A run **passes** when every stage that applies to it passed: apply, hidden
tests, every stage the profile defines and, when it defines an app and a load
script, the stack stages and perf. Perf thresholds are flags
(`-p95-tolerance 0.25`, `-p95-slack-ms 5`, `-error-rate-tolerance 0.001`); the
candidate fails perf if its p95 exceeds `base × (1 + tolerance) + slack` (each
side's p95 is the median of its per-round p95s, so one noisy round can't decide
the verdict), its 5xx rate exceeds base + tolerance, or its share of responses
with the expected status drops below base − tolerance.

### Stage statuses

| status    | meaning |
|-----------|---------|
| `pass`    | stage ran and the candidate met it |
| `fail`    | stage ran and the candidate did not meet it: a verdict on the candidate |
| `error`   | infrastructure failed (baseline didn't come up, test DB died, tool crashed, disk full): **not** a verdict; rerun |
| `skipped` | stage doesn't apply to this repo, or an earlier stage made it impossible |

`error` is how flaky machines are kept out of the score: if a test database
dies mid-suite (a pattern the profile lists under `tests.infra_errors`),
`go_test` and `hidden_tests` are `error`, not `fail`. Exclude `error` runs when
computing pass rates.

## Task file

`<workspace>/tasks/<id>/task.yaml`:

```yaml
id: issue-9-team-scope             # [a-z0-9-]; names results and compose projects
task_type: bug_fix                 # bug_fix | feature | refactor | migration
area: items                        # free text; the heat-map row
repo: api                          # api (default) | web
base_commit: 4e90ad4…              # full SHA the candidate starts from
reference_commit: ba5ab6a…         # optional: known-good solution, the default candidate
description: |                     # the prompt an implementing agent will receive
  …
hidden_tests:                      # copied into the candidate after it is applied
  - from: testdata/items_test.go   # relative to the task file; keep under testdata/
    to: handlers/items_test.go     # relative to the repo root; overwrites any same-named file
load:                              # optional; overrides the profile's load, field by field
  script: k6/load.js               # relative to the profile dir
  vus: 10
  duration: 10s                    # per round
  rounds: 3                        # rounds per stack, interleaved
```

Unknown keys are rejected. Hidden tests live under a `testdata/` dir so Go
tooling ignores them.

### Making a new task

1. Pick `base_commit` (the parent of a real fix, or the main branch for a new
   issue).
2. Write hidden tests that **fail at `base_commit`** for the right reason and
   include a control that passes there (so "reject everything" can't pass).
3. Run `fixlab run -task …` with the known-good fix as `reference_commit`: it
   must pass. Then try at least one plausible wrong fix (`-candidate-patch`): it
   must fail. A task whose wrong fixes pass measures nothing.

`reference_commit` must stay reachable. If the fix branch is squash-merged and
deleted, point it at the merge commit.

fixlab can do steps 2 and 3 with agents, from `tasks/<id>/issue-as-filed.md`
and the maintainer-approved `refined.md`. A person reviews the tests before
anything else runs on them. Every step is a command; agent steps save their
prompt and transcript under `results/<id>/<id>-<step>-<time>/`. Each reads its
prompt template from the workspace (`-prompt`, defaults below).

```bash
# Draft the spec, read-only at the base (prompts/refine-v1.md): verified
# findings, expected behaviour and numbered open questions. Writes
# refined-draft.md next to the issue.
fixlab refine -id issue-13-low-stock -base abc1234 \
  -issue tasks/issue-13-low-stock/issue-as-filed.md

# After the maintainer answers the open questions, merge the draft and the
# decisions into refined.md (prompts/finalise-v1.md). Every question of the
# issue must be decided. Editorial choices the agent made go to
# finalise-notes.md for review; they are not part of the spec.
fixlab finalise -task-dir tasks/issue-13-low-stock -base abc1234 -decisions decisions.json

# Tests from the spec, on the unfixed base (prompts/write-tests-v1.md). Writes
# testdata/, task.yaml (no reference yet) and test-kinds.yaml, and checks on the
# base: contract and concurrency tests fail, control tests pass.
fixlab write-tests -task-dir tasks/issue-13-low-stock \
  -base abc1234 -test-file handlers/issue13_low_stock_test.go -area items

# Review the tests against the spec and their base results, read-only
# (prompts/review-tests-v1.md): tests that would reject a correct fix, wrong
# failure reasons, gaps, flakiness. Reports only; saved as review.md.
fixlab review-tests -task tasks/issue-13-low-stock/task.yaml

# Then search for a reference: run the default strategy under
# -experiment reference-search, at most 3 attempts per round and 2 rounds
# (see "Attempt cap" below), and promote a passing attempt: a local branch
# off base with the tests, then the attempt's diff, recorded as
# reference_commit. The promoted attempt grades the old task version, so it
# stays out of the benchmark.
fixlab run -task tasks/issue-13-low-stock/task.yaml \
  -strategy strategies/refine-then-implement.yaml -experiment reference-search -runs 3
fixlab promote -task tasks/issue-13-low-stock/task.yaml \
  -result results/issue-13-low-stock/<run_id>.json -branch fix/13-low-stock

# Plausible wrong fixes, each starting from the reference without the hidden
# tests (prompts/write-control-v1.md). A control the tests catch is kept as
# controls/pN-<slug>.diff, headed by its mistake and the tests it fails; one
# they miss fails the command, because the tests need another look.
fixlab controls -task tasks/issue-13-low-stock/task.yaml -n 2
```

Test names carry their kind (`TestIssueN_…OtherLab…` for isolation,
`…Concurrent…`, `…Control…`, else contract), and helpers start with `hN`, so
the hidden tests can't collide with a candidate's own.

## Result schema (`schema_version: 4`)

One file per run: `<workspace>/results/<task_id>/<run_id>.json`, with logs and
raw outputs (`go-test.json`, k6 summaries, every command's output) in
`<workspace>/results/<task_id>/<run_id>/`. All keys are always present; perf
numbers are `null` when the load test didn't run.

| field | type | notes |
|---|---|---|
| `schema_version` | int | bumped on any field change |
| `task_id`, `area`, `task_type`, `repo` | string | from the task file; heat-map axes are `area` × `task_type` |
| `run`, `runs` | int | this run's number (1-based) and the `-runs` total |
| `run_id` | string | unique; also the compose project / worktree name |
| `implementer` | string | `ref:<sha>`, `patch:<file>` or the agent name |
| `base_commit`, `started_at` | string | `started_at` is RFC 3339 UTC |
| `duration_sec` | number | wall time including teardown of nothing but this run |
| `pass` | bool | every applicable stage passed |
| `failure_stage`, `error` | string | first stage that failed/errored and its message ("" on pass) |
| `stage_apply` … `stage_perf` | string | `pass` / `fail` / `error` / `skipped`, see above |
| `go_tests_total/passed/failed/skipped` | int | every test incl. subtests, from `go test -json` |
| `hidden_tests_total/passed/failed` | int | top-level hidden Go tests (web: hidden files) |
| `frontend_tests_total/passed/failed` | int | Vitest (web tasks) |
| `lint_new_findings` | int | lint findings introduced by the candidate |
| `typecheck_new_errors` | int | TypeScript errors introduced by the candidate (web) |
| `baseline_p95_ms`, `candidate_p95_ms` | number\|null | median over rounds of k6 `http_req_duration` p95 |
| `p95_delta_pct` | number\|null | (cand − base) / base × 100 |
| `baseline_error_rate`, `candidate_error_rate` | number\|null | 5xx responses / all responses, all rounds |
| `baseline_checks_rate`, `candidate_checks_rate` | number\|null | responses with the status the script expected / all, all rounds |
| `baseline_requests`, `candidate_requests` | int | requests k6 completed, all rounds |
| `experiment` | string | `-experiment` label, "" if none |
| `diff_files`, `diff_lines_added`, `diff_lines_removed` | int | candidate change size, before hidden tests |
| `diff_files_outside_reference` | int\|null | changed files the reference fix didn't touch; null without `reference_commit` |
| `implementer_kind` | string | `ref`, `patch` or `agent` |
| `model`, `effort`, `agent_cli_version` | string | agent attempts only |
| `prompt_variant`, `prompt_sha256` | string | template name and hash of the rendered prompt |
| `claude_md_variant`, `claude_md_sha256` | string | `repo`, `none` or the variant file's name; hash of what the agent saw |
| `agent_outcome` | string | CLI result subtype (`success`, `error_max_turns`, `error_max_budget_usd`, …) |
| `agent_cost_usd`, `agent_duration_sec` | number\|null | as reported by the CLI; the cost is the API list price of the tokens used, not a charge: on a subscription a run is not billed, it uses plan quota |
| `agent_input_tokens`, `agent_output_tokens`, `agent_cache_read_tokens`, `agent_cache_write_tokens`, `agent_turns` | int\|null | |
| `agent_denied_path_refs` | int\|null | mentions of answer-holding dirs in the transcript; must be 0 |
| `harness_version` | string | the commit fixlab was built from (`+dirty` if uncommitted), or its module version |
| `task_sha256` | string | hash of task.yaml and hidden tests |
| `grading_sha256` | string | hash of the base commit and hidden tests only (not the rest of task.yaml); with `prompt_sha256` it keys the attempt cap |
| `cap_override` | string | the `-past-cap` reason when the attempt ran past the attempt cap; "" otherwise |
| `regraded_from` | string | `run_id` of the attempt whose diff was replayed; "" for fresh attempts |
| `profile`, `profile_sha256` | string | the project profile the run was graded with and the hash of its files; "" for web tasks |

Heat map: `jq -s 'map(select(.stage_go_test != "error" and .stage_stack_up != "error"))' results/*/*.json`,
then group by `area`, `task_type` and average `pass`.

## Project profile

A project tells fixlab everything specific about itself in `.eval/` at its
repo root: `profile.yaml` and the files it names. fixlab reads it from the
task's `base_commit`, so an old task keeps the profile it was written
against; a commit from before the project had one uses the checkout's
`.eval/`. Each attempt records the profile's name and a hash of its files
(`profile`, `profile_sha256`), so a changed profile shows up in the history
like changed hidden tests.

A Go API backed by Postgres, whose clients sign in through Clerk
(`testdata/example/.eval/profile.yaml` is a complete one):

```yaml
name: my-api
toolchain:
  env: { GOTOOLCHAIN: local }        # added to every stage command

app:                                 # built from the worktree under test
  dockerfile: api.Dockerfile         # extra build context "profile" = this dir (COPY --from=profile …)
  port: 8080
  health: /health                    # probed with wget inside the container
  env:                               # templates over service outputs
    DATABASE_URL: "{{ .postgres.url }}"
    CLERK_PUBLISHABLE_KEY: "{{ .clerk.publishable_key }}"
    SSL_CERT_FILE: "{{ .clerk.ca_file }}"

services:                            # built-in modules
  postgres: { module: postgres, version: 16.4-alpine3.20 }
  clerk: { module: jwt-issuer, preset: clerk, users: users.json }

seed: { sql: seed.sql, into: postgres }
load:
  script: k6/load.js                 # writes /out/summary.json (see below)
  vus: 10
  duration: 10s
  rounds: 3
  env: { API_URL: "{{ .app.url }}", TOKENS_FILE: "{{ .clerk.tokens_file }}" }

stages:                              # shell commands run in the worktree, in order
  - { name: build, run: go build ./..., gate: true }
  - { name: vet, run: go vet ./... }
  - { name: lint, run: "go run …staticcheck@2026.2.1 -f json ./...", compare: baseline, results: staticcheck-json }
  - name: test
    each: go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...
    run: go test -json -count=1 -timeout=15m {item}
    results: go-json

tests:
  infra_errors: [failed to start postgres container, "connect: connection refused"]
agent:
  allow: [Bash(go build:*), Bash(go test:*), Bash(go vet:*)]
```

Unknown keys are rejected, every path must stay inside `.eval/`, and env
templates are checked against the modules' outputs when the profile is read.

**Stages.** `name` is `build`, `vet`, `lint` or `test` (each reports to its
result field); a `test` stage is required, since hidden tests are graded from
its output. A stage without `results` is decided by its exit code. `gate: true`
skips every later stage on failure. `each` runs `run` once per output line,
with `{item}` replaced. `compare: baseline` runs the stage on the base worktree
too and fails only on findings the candidate added. `timeout` is per command
(default 20m). Readers: `go-json` (`go test -json`) for `test`,
`staticcheck-json` for `lint`. The `-go` binary's dir goes first on `PATH`, so
`go` in a stage is the Go fixlab was told to use. Running `go test` once per
package keeps testcontainers sessions from reaping each other's databases.

**Modules.**

| Module | Fields | Outputs |
|---|---|---|
| `postgres` | `version` (image tag, required) | `url`, `host`; tmpfs data, credentials `eval`/`eval`, database `eval` |
| `jwt-issuer` | `preset: clerk`, `users` (optional JSON file) | `host`, `jwks_url`, `ca_file`, `tokens_file`, and with `clerk`, `publishable_key` |

`jwt-issuer` stands in for a hosted identity provider: nginx serving a JWKS
over HTTPS at `<service>.eval.local`, with keys and a self-signed certificate
generated per run. Give the app `ca_file` as its only trust root and any call
to a real external HTTPS service fails instead of leaving the machine. The
`users` file is `[{"claims": {...}, "data": {...}}]`; `tokens_file` is one
RS256 token per user, `[{...data, "token": "…"}]`. The `clerk` preset's
`publishable_key` decodes to the issuer's host, so Clerk SDKs fetch its JWKS.
`.app.url` (`http://app:<port>`) is available to `load.env`.

**Load script contract.** k6 runs the script from `/profile/<script>` with
`VUS` and `DURATION` set, plus `load.env`; its `handleSummary` must write
`/out/summary.json` with `p95_ms`, `error_rate` (5xx share), `requests` and
`checks_pass_rate`.

**Seed data** should be fully synthetic and deterministic (fixed IDs, dates
relative to a fixed day, emails on the reserved `.test` TLD), so baseline and
candidate see the same data and no real data ever reaches a run.

### Using the stack directly: `fixlab env`

```
fixlab env up    -project ../my-api
fixlab env down  -project ../my-api
fixlab env check -project ../my-api
```

`env up` builds the app from the project dir as it is (uncommitted changes
included), starts the services, seeds, and prints the app's URL on
`127.0.0.1` and the tokens files; it replaces a running env of the same
profile and project dir (two checkouts of one project get separate envs).
State and logs live in `<user cache dir>/fixlab/env/<profile>-<hash of project dir>/`,
and `env.json` there records the URL. `env check` is how a profile is accepted:
every stage passes (findings are counted, not judged: there is no baseline),
the stack comes up healthy and seeds, and one load round makes requests with
no 5xx and every check passing; then the stack is removed. `fixlab clean`
removes envs too.

## Agent runs

```
fixlab run -task tasks/<task>/task.yaml -runs 3 -agent claude -model sonnet -experiment baseline
#   -effort high  -prompt prompts/issue-v1.md  -claude-md path/to/variant.md|none  -agent-budget-usd 5
```

Each run invokes Claude Code headless (`claude -p … --output-format stream-json`),
so `-runs 3` means three independent agent attempts. Set `CLAUDE_BIN` to the CLI
and `CLAUDE_CONFIG_DIR` to the account it should use (or pass `-agent-cli`,
`-agent-config-dir`). Agent runs need a history (see [History](#history)) so the
attempt cap can count them; pass `-no-history` to run without one.

**The agent cannot see the answer.** It does not work in a git worktree (those
share refs, so the reference fix on another branch would be one `git show`
away). It gets a standalone export of `base_commit`: tracked files only, the
workspace removed when it lives in the repo, a fresh single-commit repo, in a
temp dir outside every repo. On top of that:

- permission mode `dontAsk` with a tool allowlist: read/edit files and
  `git diff/status` (`internal/implement.CoreTools`), plus the profile's
  `agent.allow`; everything else is denied;
- read tools are denied on the repos' parent dirs, the workspace and the
  history dir;
- only the repo's own settings load (`--setting-sources project`), so user-level
  hooks, plugins and permissions don't change results between machines;
- `--strict-mcp-config` (no MCP servers), `--no-session-persistence`;
- the transcript is scanned afterwards and `agent_denied_path_refs` counts any
  mention of a denied dir. Read rules are not a sandbox, so a non-zero count
  means review the attempt before trusting it.

The agent's diff (committed or not, new files included) is applied to the
candidate worktree and graded like any other candidate. Hidden tests are copied
in afterwards. A CLI that can't start or authenticate is an infra `error`; an
agent that changes nothing, times out or produces a diff that doesn't apply is
a `fail`.

**Variables you can experiment with** are recorded with every attempt:

| Variable | Flag | Recorded as |
|---|---|---|
| model | `-model` | `model` (the id the CLI reports) |
| effort | `-effort` | `effort` |
| prompt | `-prompt prompts/<name>.md` (Go template over the task: `.Description`, `.Refined`, `.ID`, `.Area`, `.TaskType`, `.Repo`) | `prompt_variant`, `prompt_sha256`; `prompt.md` in the attempt |
| CLAUDE.md | `-claude-md <file>` replaces the repo's, `none` removes it | `claude_md_variant`, `claude_md_sha256` |
| CLI version | | `agent_cli_version` |

Adding another agent means implementing `implement.Implementer`
(`Implement(ctx, ws) (Info, error)`) and wiring it in `cmd/fixlab/main.go`.

### Plugins

`-agent-plugin DIR` (repeatable) loads plugins into the agent session with
`--plugin-dir`, and adds the Skill tool to what the agent may use. Plugins
installed for your account never load, so a variant is only what the command
line says. Each attempt records `agent_plugins` (name@version),
`agent_plugins_sha256` (hash of the plugin files) and `agent_skills_used`
(skills the agent invoked). A requested plugin missing from the session is an
infra error, not a silent run without it.

`-agent-allow TOOL` (repeatable) adds tools to the default set, e.g. `Task` for
workflows that delegate to subagents. The full tool list is in each attempt's
`manifest.json`. Subagents run under the same permissions and read-deny rules,
and their usage counts toward the attempt's tokens.

`-agent-setup "cmd"` runs a shell command in the agent's snapshot before it
starts, after the CLAUDE.md variant is in place, and logs it to
`agent-setup.log`; `-agent-ignore PATH` adds paths to the snapshot's
`.git/info/exclude`, so what setup generates (and the agent regenerates) never
reaches the graded diff. Example, a code graph built from the base commit only:

```
fixlab run -task tasks/<task>/task.yaml -agent claude -experiment graphify \
  -agent-setup "graphify update ." -agent-ignore graphify-out/ \
  -agent-allow "Bash(graphify query:*)" -agent-allow "Bash(graphify update:*)"
```

### Strategies

A strategy is the whole agent configuration in one file,
`<workspace>/strategies/<name>.yaml`: model, effort, prompt, CLAUDE.md,
plugins, extra tools, setup, ignored paths, budget and timeout. Paths in it
are relative to the workspace. `-strategy FILE` runs it; it cannot be mixed
with the individual agent flags, and every attempt records `strategy` and
`strategy_sha256`, so what ran is never in doubt. The experiment label
defaults to the strategy name.

```yaml
name: refine-then-implement
description: Issue plus the maintainer-approved refined spec.
model: claude-opus-5-5
prompt: prompts/refined-v1.md
claude_md: claude-md/trimmed.md
budget_usd: 10
timeout: 45m
```

A prompt that uses `{{.Refined}}` needs the task's `refined.md` (the issue
rewritten by `fixlab refine`, then `fixlab finalise` with the maintainer's
decisions) and refuses to run without one. Like `test-kinds.yaml`,
`refined.md` is not part of the task version.

### Benchmark: trying a change against the default

`<workspace>/benchmark.yaml` lists the benchmark tasks, the runs per task, the
default strategy, the rule a challenger must meet, and a dated log of every
change to the default.

```
fixlab bench -strategy my-idea              # every benchmark task, 3 runs each (-dry-run to preview)
fixlab compare -strategy my-idea            # task by task against the default, with a verdict
fixlab report                               # the same comparison, plus the default's history
```

To try something new, copy a strategy file, change one thing, run
`fixlab bench` with it, then `fixlab compare`. The verdict is one of:

| Verdict | When |
|---|---|
| `adopt` | on every benchmark task, with at least `min_runs` graded runs each: pass rate no lower, security tests passed in no fewer runs, and average tokens at most `max_token_increase` higher, unless some task's pass rate went up |
| `keep-default` | any of those fails; the reasons say which |
| `not-enough-data` | a task lacks `min_runs` graded runs for either strategy |

Adopting means a change that edits `default` and adds a `history` entry with
the evidence. A task whose prompt needs `refined.md` it doesn't have is
skipped by `fixlab bench`, with a message.

Attempts are grouped by **what ran**, not by their label: model, effort,
and the hashes of the rendered prompt, the CLAUDE.md the agent saw and its
plugins, plus the setup command. An earlier attempt whose configuration is
exactly what a strategy would run on its task counts as that strategy, with
no reruns.

### Attempt cap

`run` refuses agent attempts past a fixed budget, before any agent starts
(`internal/budget`), so neither a benchmark nor a reference search becomes
"rerun until it passes". Attempts are grouped by what they face: the rendered
prompt (which carries the issue or spec) and the grading (base commit and
hidden tests). Moving `reference_commit` changes neither.

- **Benchmark arms** are measurements: each experiment gets 3 attempts per
  prompt and grading. More would bias the pass rate; regrade or compare instead.
- **Reference search** (`-experiment reference-search`): rounds of at most 3
  attempts. After a round without a pass, triage the failures (spec, tests or
  implementation). A second round needs a changed spec or tests, and there
  are at most 2 rounds; after that the maintainer decides: split the issue,
  write the fix by hand, or drop the task. A round with a pass ends the search:
  promote it.
- Infrastructure errors, regrades and runs of a ref or patch don't count, so a
  round interrupted by infra errors can be topped up.
- `-past-cap "<reason>"` runs anyway, only when the maintainer agreed; each
  attempt records the reason in `cap_override`.

The cap counts the attempts in the history; `-no-history` runs aren't counted.

### Robustness

- **Grading retries.** An infra error after the candidate was applied (test DB
  lost, stack down, host stall) grades the same candidate again by replaying
  its diff, up to `-infra-retries` (default 2) times; the agent never runs
  twice. `infra_retries` and `infra_retry_errors` record it.
- **Perf confirmation.** A failing perf verdict is re-checked with a second,
  fresh load test and fails only if both do (`perf_reruns`).
- **Only allowed tools.** The agent session gets `--tools` with just the
  built-in tools its allow-list names, which saves ~10k tokens per request.
- **No production credentials.** Everything that runs agent-written code (the
  agent session, builds, tests, the local stack and the write-tests base check)
  gets `implement.SafeEnv()`: no database URL, auth-provider, cloud or GitHub
  tokens, and gcloud and Pulumi point at an empty config dir.

## History

Every run can also be recorded in a history repo (`-history DIR`, or env
`FIXLAB_HISTORY`), one per project, under `attempts/<task_id>/<run_id>/`:
`result.json`, `manifest.json`, `candidate.diff`, `prompt.md`,
`transcript.jsonl`, raw test output, k6 rounds and logs. Each attempt is
committed there (`-history-commit`, default on; never pushed). Keep it
private: attempts hold your code's diffs and full agent transcripts.

```
fixlab history                  # pass rate per task × experiment × implementer × prompt × CLAUDE.md × effort
fixlab history -csv > a.csv     # every attempt, all result fields
fixlab history -task issue-9-team-scope
```

Pass rates exclude infra errors. The summary groups by task version
(`task_sha256`), so attempts graded by different hidden tests are never pooled.
Compare attempts only when `harness_version` matches too; a version ending in
`+dirty` was produced by an uncommitted fixlab.

### Regrading

When a grader improves (better hidden tests, a fixlab fix), score recorded
attempts again instead of re-running, and re-paying for, the agent:

```
fixlab run -task tasks/<task>/task.yaml \
  -regrade <history>/attempts/<task>/<run_id> -experiment <original experiment>
```

The recorded diff is replayed. Model, prompt, CLAUDE.md, cost and transcript
are carried over from the original attempt, and `regraded_from` records its
`run_id`.

### Attempts that didn't fully pass

```
fixlab failures                                  # every task
fixlab failures -task issue-10-x -variant baseline
```

Lists every agent attempt and reference fix on a task's current tests that
isn't a full pass, newest first: its stage, the hidden tests it missed grouped
by kind (security, contract, concurrency, control), and its folder in the
history (diff, transcript, test output). Wrong-fix controls are left out,
since failing is their job. An infra error graded nothing, so it shows the
error and how many retries it took instead of tests. After the list, each
task's **hardest hidden tests**: how often agent attempts failed each one.
The report has the same two views.

### Report and task size

```
fixlab report                   # writes <history>/report.html
fixlab report -o /tmp/r.html
```

One self-contained HTML file: no network, no CDN, the data is embedded.
Open it in a browser. It shows pass rates per task and variant, which hidden
tests each attempt passed, agent token usage (and its list-price value), p95
change, diff size, and every recorded attempt.

It also answers how big a task agents can take on alone. Each task is sized
from its reference fix (`base_commit..reference_commit`), with test files
counted apart:

| Size | Production lines | Files | Functions touched |
|---|---|---|---|
| XS | ≤ 25 | 1 | 1 |
| S | ≤ 100 | ≤ 2 | ≤ 3 |
| M | ≤ 300 | ≤ 5 | ≤ 8 |
| L | ≤ 800 | ≤ 12 | ≤ 20 |
| XL | more | | |

A task's class is the largest one any measure reaches. Functions are read
from git's hunk headers, so they are approximate (a new top-level function
counts as the one before it). The report pools agent attempts per class for
the chosen variant and model, and calls a class safe when it has enough tasks
(default 3) and its pass rate meets the bar (default 80%). The threshold is
the largest class that, with every smaller class that has data, is safe.

A pass needs every hidden test, so one missed edge case scores the same as a
broken change. The report also shows partial credit (the share of hidden
tests passed) and, when the task has a `test-kinds.yaml`, results per kind of
test, e.g. security vs contract:

```yaml
# tasks/<task>/test-kinds.yaml: report only, not part of the task version
- kind: security
  label: Keeps other teams' data safe
  tests: ["*_NoCrossTeam*"]         # globs on top-level test names
- kind: contract
  label: Rejects with 404
  tests: ["*_OtherTeamItemReturns404"]
```

The size threshold can then count a pass as "every test of one kind", so you
can ask how big a task agents handle when only security has to be right.

## Known limitations

- **Latency is only meaningful as baseline vs candidate on the same machine.**
  Locally there are no cold starts, no autoscaling, no network hops to hosted
  services, and a laptop's CPU. Absolute p95 numbers say nothing about
  production.
- **Heavy host activity skews perf.** k6 runs are serialized across every
  fixlab process on the machine, whatever its workspace (a lock in the user
  cache dir, `fixlab/load-test.lock`), and interleaved in rounds, but builds,
  test suites or anything else on the machine (including other Docker
  projects) still add noise. Solo runs of an unchanged-performance fix have
  varied by about ±20% in p95 delta on a laptop; treat smaller differences as
  noise, and keep the machine idle for runs whose perf numbers you intend to
  compare.
- **Cloud IAM is not enforced.** Everything runs with no cloud identity, and
  the `jwt-issuer` JWKS accepts any token fixlab signs.
- External services the profile leaves out are not exercised; tasks touching
  them need hidden tests with fakes.
- The infra-failure detection for tests is a heuristic: the profile's
  `tests.infra_errors` patterns in the test output.
- Stages run on the host. Projects whose tests use testcontainers start their
  own containers alongside the stacks; Docker Desktop's default memory can be
  tight when several runs overlap.
- `fixlab clean` removes every `fixlab-*` compose project on the machine,
  including another workspace's running stacks.

## License

Apache License 2.0: see [LICENSE](LICENSE) and [NOTICE](NOTICE).
