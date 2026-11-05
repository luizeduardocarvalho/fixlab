---
name: eval-task
description: Take an issue through fixlab from report to merged fix - refine the spec, get the maintainer's decisions, write and review hidden tests, search for a reference fix with agents, promote it, write wrong-fix controls, validate, then open and merge the PR. Use this whenever someone wants to work an issue "through the harness" or "through fixlab", make or finish an eval task, refine an issue into a spec, write hidden or acceptance tests for a task, run a reference search, promote an attempt, write controls, validate a task, or pick up the next issue of a batch, even if they only say "do #N with fixlab" or "next issue".
---

# Eval task

A fixlab task turns one issue into two things: a **fix** that is merged with tests proving it, and a **measurement** (hidden tests, a reference fix that passes them, wrong fixes that fail them) that grades any later agent on the same issue. Both come out of the same pipeline, and a shortcut on either side shows up as a grade nobody can trust.

Finish line: the fix is merged into the integration branch, the task's `reference_commit` passes 3 out of 3 runs, every control fails, and the task directory is committed in the workspace.

## Before you start

- **Find fixlab.** This skill lives at `<fixlab>/skills/eval-task/`. Resolve the real path of this file (it may be a symlink) and go two levels up. `<fixlab>/README.md` is the reference for every command, flag and result field, so read the sections you need rather than working from memory. Run `fixlab <cmd>` if it's installed; otherwise run it the way the project does (a Makefile target or `go run …/cmd/fixlab@<version>`). Pass `-go <path>` if `go` isn't on `PATH`.
- **Find the workspace and history.** The workspace holds `tasks/`, `prompts/`, `strategies/` and `benchmark.yaml` (`-workspace`, default the current dir). The history is the private repo where attempts are recorded (`-history` or `FIXLAB_HISTORY`). Agent runs refuse to start without one unless you pass `-no-history`, and the attempt cap can't count attempts that aren't recorded.
- **The project needs a profile.** If the repo has no `.eval/profile.yaml`, use the `eval-profile` skill first.
- **Agent account.** Set `CLAUDE_BIN` and `CLAUDE_CONFIG_DIR` to the CLI and account that should pay for agent sessions.
- **Read the project's rules.** Read its CLAUDE.md or AGENTS.md for branch names, commit style, who may merge where, and anything you must never touch (production, deploys, CI config). They override this skill.

## Sharing the machine

Grading builds images, runs the suite and load-tests the code, and another session grading at the same time skews both sets of perf numbers. Before any `run`, `controls` or `write-tests`:

- Check for running fixlab processes (`pgrep -fl fixlab`) and other sessions' stacks (`docker compose ls | grep fixlab-`). If another session is grading, wait or ask.
- Never grade with two different harness builds at once. Their load-test locks may not serialise each other.
- Check free disk space. A run needs several GB, and a full disk can make Docker read-only.
- If an earlier run was killed, `fixlab clean` removes what it left. It removes **every** `fixlab-*` stack on the machine, so check first.

## The pipeline

Each step has a checkpoint. Stop at a failed checkpoint instead of pushing through it: the later steps can't repair a bad spec or a bad test.

### 1. Re-verify the issue against today's code

Issues go stale. Before writing anything, read the code the issue points at on the current integration branch and confirm each claim still holds. Look especially at work merged since the issue was filed: another fix may have done part of it or changed a shape the issue depends on (a response format, a validation rule). Trim what's already done, and note what changed under it. Pick `base_commit` = the integration branch tip.

Checkpoint: every remaining claim is confirmed at the base, with a file and line.

### 2. Refine and decide

Save the issue verbatim as `tasks/<id>/issue-as-filed.md` (`<id>` is `issue-<n>-<slug>`), then:

```
fixlab refine -id <id> -base <sha> -issue tasks/<id>/issue-as-filed.md
```

It writes `refined-draft.md`: verified findings, expected behaviour, and numbered open questions, each with a recommendation. **The maintainer answers the questions, not you.** Show them the questions with the draft's recommendations, then record their answers as the decisions file: `{"answers": [{"id", "issue": <n> or "plan", "question", "decided": true, "answer", "followedRecommendation", "note"}]}`.

```
fixlab finalise -task-dir tasks/<id> -base <sha> -decisions decisions.json
```

It writes `refined.md`, the spec everything else is graded against. Read it in full, and read `finalise-notes.md` (the editorial choices the agent made) with the maintainer. If the project tracks specs on its issues, post `refined.md` as a comment, and keep that comment in sync whenever the spec changes later.

Checkpoint: every question is decided, and the maintainer has seen the spec.

### 3. Write the hidden tests

```
fixlab write-tests -task-dir tasks/<id> -base <sha> -test-file <path/in/repo>_test.go -area <area> -type bug_fix|feature
```

It writes `testdata/`, `task.yaml` (no reference yet) and `test-kinds.yaml`, then runs the tests on the base: the red check. **Read the failure messages, not just the counts.** Each contract, security and concurrency test must fail for the reason the spec gives. A test that fails because a helper is broken, the route doesn't exist yet, or the setup crashes measures nothing. Control tests must pass on the base. If the spec says behaviour stays the same somewhere, a control should pin it.

Test names carry their kind (`…OtherLab…`/`…OtherTeam…` for isolation, `…Concurrent…`, `…Control…`, else contract) and helpers start with `hN`, so they can't collide with an agent's own tests.

### 4. Review the tests

```
fixlab review-tests -task tasks/<id>/task.yaml
```

It reports read-only: tests that would reject a correct fix, wrong failure reasons, gaps in the spec's table, flakiness. Fix what it finds, rerun the red check, and repeat until the review is clean. Ask the maintainer only when a finding is really a question about the spec.

Hidden tests are part of the task version (`grading_sha256`). Changing them later starts a new version: earlier attempts no longer count toward that version's pass rate, and the attempt cap starts over. So finish the tests here, before agents spend money on them.

Checkpoint: a clean review, and a red check where every failure has the right reason.

### 5. Search for a reference fix

Run the benchmark's default strategy (see `benchmark.yaml`):

```
fixlab run -task tasks/<id>/task.yaml -strategy strategies/<default>.yaml -experiment reference-search -runs 3
```

The attempt cap holds: rounds of at most 3 attempts, and at most 2 rounds. A round with a pass ends the search. After a round with no pass, **triage before rerunning**. `fixlab failures -task <id>` shows which hidden tests each attempt missed. Decide from the spec whether each miss is the attempt's fault, a wrong test, or an unclear spec. The spec decides between test and code: if the spec says X and the test checks X, the attempt is wrong. A second round needs a changed spec or tests. After 2 rounds, stop and let the maintainer choose: split the issue, write the fix by hand, or drop the task. Never pass `-past-cap` unless the maintainer agreed to it; the reason you pass is recorded.

An `error` status is infrastructure, not a verdict, and doesn't count toward the cap: rerun it. Check `agent_denied_path_refs` is 0 for every attempt. A non-zero count means the agent mentioned an answer-holding path, so review that attempt before trusting its pass.

### 6. Choose and promote

When several attempts pass, read their production diffs side by side and pick the one you would merge yourself:

- It follows the spec in every row, including error order and messages.
- It has no behaviour the spec didn't ask for.
- It has the smallest clear change, in the style of the surrounding code.
- It has no lint findings and doesn't weaken or delete existing tests. If the spec told the implementer to update an existing test, check that it updated the test rather than deleting it.

Say why you chose it.

```
fixlab promote -task tasks/<id>/task.yaml -result results/<id>/<run_id>.json -branch fix/<n>-<slug> -subject "fix: <what it does, lowercase imperative>"
```

That creates a local branch off the base with two commits: the hidden tests, then the attempt's change. Then add a third commit by hand: delete the attempt's own tests that repeat the acceptance tests, and say in the message which file covers them. First compare the cases one by one, and keep any test that checks something the acceptance tests don't. Finally, set `reference_commit` in `task.yaml` to the branch tip.

### 7. Write controls

```
fixlab controls -task tasks/<id>/task.yaml -n 3
```

Each control is a plausible wrong fix, written from the reference. The ones the hidden tests catch are kept as `controls/pN-<slug>.diff`. If one is missed, the tests have a gap: add a test for it (back to the red check in step 3) and regrade the reference. A task whose wrong fixes pass measures nothing.

### 8. Validate

Run these one after another, never in parallel:

1. **Reference, 3 runs:** `fixlab run -task tasks/<id>/task.yaml -runs 3`. All 3 must pass, perf included. One fail is a flaky test or a perf problem: find out which.
2. **Each control, fully graded:** `fixlab run -task … -candidate-patch tasks/<id>/controls/<p>.diff`. Each must fail at `hidden_tests`.
3. **The project's own full suite** on the fix branch, the way its CI or pre-push hook runs it.
4. **Optional, if the project gathers benchmark data:** an arm with another strategy (for example a cheaper model). It counts toward the attempt cap. When one of its attempts fails, find out why. A failure may be the arm's fault, or it may point to a test that rejects a correct fix.

### 9. Ship

- Push the fix branch and open a PR into the integration branch. The body has the summary in the spec's terms, the affected modules, the commit list (tests, promoted attempt with its `run_id` and strategy, pruned tests), the validation numbers, any new env vars or migrations, and an example request and response.
- Merge only if the project's rules let you merge into that branch, and never a PR that changes CI or deploy config unless they explicitly allow it. Otherwise hand it to the maintainer.
- Keep `reference_commit` reachable. Don't delete the fix branch after a squash merge; if it must go, point `reference_commit` at the merge commit.
- Commit the task directory in the workspace (spec, tests, `task.yaml`, controls) with a message saying what changed and why.
- If the merge changed something other tasks depend on (a response shape, a validation rule), update those tasks' specs and tests now, before anyone grades them against the old shape. Downstream clients may need a matching change too. Say so, and note the order the releases must ship in.

## Reporting

At every checkpoint and at the end, tell the user plainly:

- what was decided and by whom
- the numbers: red check, attempts per round, reference runs, controls caught
- which attempt you promoted and why
- the PR link and merge state
- what's left for them to do

Report failures as failures, with the stage and the message. Skipped is not passed.
