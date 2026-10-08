# rmm-agent

The Linexus **infrastructure (RMM) agent** — a small, static Go binary that runs
on a managed host, relays inventory and logs, and applies changes dispatched
from Daedalus IT.

> This is distinct from the Rust crate at the repository root, which is the
> economy line's **edge-telemetry** agent for Dignifundus solar/biodigester
> hardware. This Go module manages conventional IT infrastructure. The two share
> the "agent" name and repo but are independent.

## What it does

Nexus is the only service the agent talks to. On each cycle it:

1. **Enrolls** once (persisting its assigned id to a state file) and **reports
   facts** — hostname, OS, kernel, arch, CPU/memory/disk, uptime — read from
   native Linux sources (`/proc`, `statfs`), never by shelling out.
2. **Heartbeats** for liveness.
3. **Polls** Nexus for tasks targeting it and **executes** each plan step.
4. **Ships logs and reports the result** back through Nexus (which relays logs to
   the Logger and updates task status). Daedalus IT then sees live health and the
   task's journal.

## Plan step vocabulary

The agent implements the action verbs the orchestrator emits
(`linexus-orch/src/plan.rs`):

| Action | Behavior |
|---|---|
| `command.run` | Runs `params.command` via `/bin/sh -c`, captures combined output |
| `system.reboot` / `system.power_off` / `system.power_on` | **Suppressed by default** (dry-run). Set `AGENT_ALLOW_DESTRUCTIVE=1` to enable; power-on needs out-of-band control |
| `role.provision` | Acknowledged stub — real convergence (packages/services/files) is a later phase |
| anything else | Skipped with a note |

A failed **critical** step aborts the rest of the plan.

## Reporting a result

When a task's plan has run, the agent posts one result to
`POST /api/v1/agents/{id}/tasks/{taskId}/result`:

```json
{
  "status": "failed",
  "error": "exit status 3",
  "message": "executed deploy (3 steps)",
  "exitCode": 3,
  "output": "==> file.write [success]\nwrote /etc/app.env\n==> command.run [failed]\n...\nerror: exit status 3",
  "steps": [
    {"id": "s1", "action": "file.write",     "status": "success", "changed": true,  "output": "wrote /etc/app.env", "error": ""},
    {"id": "s2", "action": "command.run",    "status": "failed",  "changed": true,  "output": "...",                "error": "exit status 3"},
    {"id": "s3", "action": "service.ensure", "status": "skipped", "changed": false, "output": "",                   "error": ""}
  ]
}
```

| Field | Meaning |
|---|---|
| `status` | `success` when every step that ran succeeded, else `failed` |
| `error` | the first failing step's error (omitted on success) |
| `message` | one-line summary |
| `exitCode` | `0` when every **critical** step succeeded; otherwise the first failing critical step's exit code — the command's exit status for `command.run`, `1` for anything else. A non-critical failure makes `status` `failed` but leaves `exitCode` at `0` |
| `output` | each step's output (and error) under a `==> action [status]` header, in plan order; capped at 64 KiB, keeping the tail behind a `[... truncated N bytes ...]` marker |
| `steps` | one entry per planned step, in order: `status` is `success`, `failed`, or `skipped` (not run because an earlier critical step failed); `output` is capped at 16 KiB and `error` at 4 KiB, the same way |

`status`, `error` and `message` are the original protocol; `exitCode`, `output`
and `steps` were added for Nexus's `GET /api/v1/tasks/{id}` and are optional on
its side, so an older Nexus ignores them. Each step's outcome is also shipped
as a log line (`POST /api/v1/agents/{id}/logs`) before the result is posted.

## Configuration (environment)

| Var | Default | Meaning |
|---|---|---|
| `NEXUS_URL` | `http://127.0.0.1:5150` | Nexus gateway base URL |
| `AGENT_TOKEN` | _(none)_ | Bearer token presented to Nexus |
| `AGENT_HOSTGROUP` | _(none)_ | Enrollment hint, e.g. `web-prod` |
| `AGENT_STATE_FILE` | `linexus-agent-state.json` | Where the assigned agent id is persisted |
| `AGENT_POLL_INTERVAL` | `10s` | Task poll cadence |
| `AGENT_HEARTBEAT_INTERVAL` | `30s` | Heartbeat cadence |
| `AGENT_ALLOW_DESTRUCTIVE` | `false` | Permit reboot/power actions |
| `AGENT_ONCE` | `false` | Run a single cycle then exit (testing / one-shot) |

## Build & run

```sh
go build -o rmm-agent .
AGENT_TOKEN=<nexus-api-key> NEXUS_URL=https://nexus.internal ./rmm-agent
```

Stdlib only — no external dependencies. Cross-compile with `GOOS`/`GOARCH`.
