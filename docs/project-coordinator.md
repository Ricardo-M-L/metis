# Durable project coordinator

METIS has two levels of task state:

- `TaskCreate` / `TaskUpdate` are a small checklist for the current chat
  session.
- A project run is a workspace-bound dependency graph that survives a session
  switch, Desktop restart, and independent CLI worker process.

The durable graph lives under `~/.metis/project-coordinator/<workspace-hash>/`.
Its run files are private (`0600`) and their directories are private (`0700`).
The hash keeps project paths out of filenames; every record still verifies its
canonical workspace before METIS loads it.

## Lifecycle

Create a project run with a concrete objective:

```sh
metis coordinator create --cwd /absolute/project/path --goal "Ship the migration"
```

METIS creates a conservative four-stage graph:

```text
research → synthesis → implementation → verification
```

The initial plan is deliberately sequential. After research gives evidence, a
coordinator may add independent work with `metis coordinator add`; this avoids
inventing parallel edits before the codebase and file ownership are understood.

```sh
metis coordinator add <run-id> \
  --cwd /absolute/project/path \
  --phase implementation \
  --subject "Migrate the API client" \
  --prompt "Update the client and its focused tests." \
  --depends-on synthesis
```

`claim` atomically reserves one ready work item and returns its prompt. This
is suitable for an external worker or integration:

```sh
metis coordinator claim <run-id> --cwd /absolute/project/path \
  --worker ci-worker-1 --prompt --json
metis coordinator complete <run-id> research --cwd /absolute/project/path \
  --worker ci-worker-1 --output "Repository inspected; focused tests: ..."
```

The built-in worker bridge performs the same lifecycle around a real METIS
agent turn:

```sh
metis coordinator run <run-id> --cwd /absolute/project/path --until-idle
metis coordinator status <run-id> --cwd /absolute/project/path
```

Each claim has a lease. If a process dies, the next status, claim, or run
reclaims the expired item and records `worker_lease_expired`; it never remains
silently stuck in `running`.

## Environment-aware recovery

Before an `Agent` creates a worktree, METIS probes the requested workspace for
Git state and linked-worktree state. A non-Git directory combined with
`isolation: "worktree"` is unambiguous: METIS records
`worktree_requires_git` in the private execution profile and continues once in
the same directory with direct execution. The project worker prompt receives
that rule on later work so another worker does not rediscover the same error.

Nested worktrees, invalid paths, unknown provider errors, and other ambiguous
failures remain blocked. They are recorded with a stable code and suggested
action, and require an explicit `recover` after the cause is fixed. Known
automatic recovery is bounded by each work item's attempt budget. After that
budget, `recover --force` permits one reviewed final retry; it cannot be used
repeatedly to make the same failure loop forever.

## Agent mode

`metis --coordinator chat` exposes the `ProjectCoordinator` tool to the
team-lead model. The model can create and expand the same project graph, claim
an item before it dispatches a teammate, and then record completion or a
specific failure. `claim_next` returns a bounded prompt containing only the
project goal, execution-environment rules, and direct prerequisite evidence.

This leaves the project graph as the source of truth while transcripts remain
the detailed reasoning record.
