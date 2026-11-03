---
name: eval-profile
description: Onboard a repository into fixlab by writing its .eval/ profile (profile.yaml, Dockerfile, overlay, seed, load script) and proving it with `fixlab env check`. Studies how the project really builds, tests and runs and which services it needs, asks the user only what the code can't answer, and reports honestly when the project needs something fixlab can't do yet. Use this whenever someone wants a project set up for fixlab or an eval harness, wants to grade or benchmark agent fixes on a new repo, asks for a reproducible local environment or stack for a project "the way fixlab runs it", mentions .eval/, profile.yaml, `fixlab env up` or `env check`, or says things like "make this repo runnable for evals" or "set up the environment for testing", even if they never say "profile".
---

# Eval profile

A project joins fixlab through one folder at its root: `.eval/`. It holds `profile.yaml` and the files it names. From it fixlab builds and tests the project, runs its stack (the app plus the services it needs), seeds it, load-tests it, and tells an implementing agent which commands it may run. The same profile gives a person or an agent a running copy of the project with `fixlab env up`.

Your job is to produce a `.eval/` that **passes `fixlab env check` on the unchanged project**, or to say clearly why the project can't have one yet. A profile that only looks right is worse than none: every grade made with it would be wrong in ways nobody notices. So the check is the finish line, not a formality.

## Find fixlab and its reference

This skill lives in the fixlab repo, at `<fixlab>/skills/eval-profile/`. Resolve the real path of this file (it may be reached through a symlink) and go two levels up to get the fixlab repo. Then:

- Read the **"Project profile"** section of `<fixlab>/README.md`. It is the authoritative description of every field, module, template output and the load-script contract. Don't work from memory: the format grows, and the README changes with it.
- Look at the example profile, `<fixlab>/testdata/example/.eval/` (`profile.yaml`, `api.Dockerfile`, `users.json`, `seed.sql`, `k6/load.js`). It is small on purpose: a real profile also comments its non-obvious lines.

Run `fixlab env check -project <repo>` if fixlab is installed, else `go run ./cmd/fixlab env check -project <repo>` from the fixlab repo. If `go` isn't on PATH, pass `-go <path>` (or set `GO`).

## 1. Survey the project (read, don't ask)

Answer these from the code before asking the user anything. Note the file and line for each answer; you'll cite them in the report.

- **Toolchain and commands.** CI config (`.github/workflows`, etc.) is the most reliable record of how the project is really built and tested; then Makefile, package scripts, README. Find the build, static check, lint and test commands, and the versions they pin.
- **Tests.** Do they need Docker (testcontainers), a database, network, or env vars? Run the test command once to see. Which failure messages mean the test infrastructure broke rather than the code (a database that went away, a container that didn't start)? Those become `tests.infra_errors`.
- **How the app runs in production.** Dockerfile, Procfile, serverless entrypoint, `main`. Port, startup command, migrations (when and how they run). A serverless function usually needs a small local entrypoint added at build time, from an `overlay/` dir in `.eval/`.
- **Health.** An unauthenticated route that answers 2xx once the app is ready. Find it in the router; if there isn't one, a cheap public GET will do. fixlab probes it with `wget` inside the app container, so the runtime image needs `wget` (busybox/alpine have it).
- **Configuration.** Every environment variable the app reads, and what happens when each is unset: crash at startup, a warning, a mock, or failure only on the call that needs it.
- **External dependencies.** Database, cache, queue, identity provider, email, payments, cloud APIs. For auth, find how tokens are verified (a JWKS URL, a key derived from a publishable key, a shared secret).
- **Existing local setups** (`docker-compose.yml`, scripts). They show what the authors run locally; fixlab renders its own Compose file, but their choices are evidence.

## 2. Decide what each dependency becomes

Put every dependency in one of four buckets, and record why:

| Bucket | When | Example |
|---|---|---|
| **Module** | fixlab has a module for it | Postgres → `postgres` |
| **Stand-in** | a module can impersonate it | Clerk/Auth0-style JWT verification → `jwt-issuer` |
| **Left unset** | you verified, in the code, that the app degrades safely without it | email falls back to a log-only mock |
| **Blocker** | the app can't start or the main paths can't run without it, and no module fits | a required queue, a second service |

"Left unset" needs evidence: cite the code that handles the missing value. Guessing here creates a stack that crashes on the first request. When the project has a `jwt-issuer` stand-in, make its `ca_file` the app's only trust root (e.g. `SSL_CERT_FILE`): then any accidental call to a real external HTTPS service fails instead of leaving the machine.

## 3. Check the project fits fixlab before writing anything

The README lists what profiles support today (test readers, stage names, modules, one app, SQL seed, `wget` health check). Compare it with your survey. If the project needs something outside that (another test output format, several app processes, file-based seed data, a service with no module), **stop and say so**: list each gap, what it would take in fixlab, and what can be done now. A stages-only profile (build, lint, test; no `app`) is often still useful, since it lets fixlab grade fixes without a stack. Offer it, but don't bend the project into a shape that misrepresents how it runs.

## 4. Ask only what the code can't answer

Collect every open question into one message, each with your proposed default, so the user can answer "yes" to most. Good questions are about intent:

- Which user flows matter most for the load test? (Default: the main read paths plus one write path a user does often.)
- Is it acceptable that `<dependency>` fails on the paths that use it? (Default: yes, if no load-test path touches it.)
- What should seed data look like: how many tenants, users, records?

Don't ask what you could have read: ports, commands, env var names, versions.

## 5. Write `.eval/` in layers, checking after each

Build up in layers, so a failure points at the layer that caused it.

1. **Stages.** `name`, `toolchain.env`, `stages` (build as a `gate`; vet/lint; a `test` stage with a results reader), `tests.infra_errors`, and `agent.allow` (the project's build/test/format commands as `Bash(<cmd>:*)` rules, nothing broader). Run `env check`. Fix until every stage passes. Lint findings are counted, not judged here, because grading compares against a baseline.
2. **Stack.** `app` (Dockerfile in `.eval/`, port, health, env), `services`, and any overlay files. Never modify the project's own files: add what the build needs from the profile dir (`COPY --from=profile overlay/ ./`). Then `env up`, call the health route, and make one authenticated request with a token from the printed tokens file, if there is an issuer. `env down` when done.
3. **Seed and load.** Seed data must be synthetic and deterministic: fixed IDs, dates relative to a fixed anchor, emails on the `.test` domain, no RNG, nothing copied from real data. If it's more than a screenful, write a small generator in `.eval/` with a test that fails when the committed output is stale. The load script follows the contract in the README (`/out/summary.json` fields). Each virtual user should act only on its own tenant's data, mix the main reads with one write path, and `check` the expected status of every response: a candidate that breaks a path should drop the check rate, not just get slower. Run `env check`.

Throughout:

- **Pin every image and tool version** (`postgres:16.4-alpine3.20`, `staticcheck@2026.2.1`, not `latest`). An unpinned version makes old grades irreproducible.
- **No real secrets.** Dummy values for anything the app requires at startup but the eval never exercises.
- **Comment the non-obvious lines of `profile.yaml`** with why: why a variable is empty, why a stage runs per package. The next person to change the profile needs the reason more than the value.

## 6. Finish

Done means `env check` passes on the unchanged project: every stage, the stack healthy and seeded, and the load round with no 5xx and every check passing. Then report:

- A table of dependencies: bucket, decision, evidence (file:line).
- What the stack doesn't exercise (unset services, paths the load script skips), so whoever grades with it knows the blind spots.
- The commands: `env check`, `env up`, `env down`, and how to regenerate seed data if there's a generator.

Don't commit unless the user asks; suggest a commit message in the repo's style.

## Sharing the machine

`env check` builds images, runs the test suite and a load round. That load competes with anything else grading on the same machine and can skew another session's performance numbers. Before running it, check for running fixlab processes (`pgrep -fl fixlab`) and other sessions' stacks (`docker compose ls | grep fixlab-`). If something is grading, wait or ask the user. Also check free disk space; a run needs several GB, and a full disk breaks Docker in ways that need a restart.
