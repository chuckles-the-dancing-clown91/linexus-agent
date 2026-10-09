# rmm-agent

The Linexus **infrastructure (RMM) agent** — a small, static Go binary that runs
on a managed host, relays inventory and logs, and applies changes dispatched
from Daedalus IT.

> This is distinct from the Rust crate at the repository root, which is the
> economy line's **edge-telemetry** agent for Dignifundus solar/biodigester
> hardware. This Go module manages conventional IT infrastructure. The two share
> the "agent" name and repo but are independent.

## Install on a managed host

Nexus serves the installer and the binaries. With an enrollment token minted
in the Hub (or `POST /api/v1/enrollment-tokens` on Nexus):

```sh
curl -fsSL $NEXUS/install/agent.sh | NEXUS_URL=$NEXUS ENROLLMENT_TOKEN=nxe_… sh
```

The script (a reference copy lives at [`deploy/install.sh`](deploy/install.sh))
downloads `rmm-agent-linux-<amd64|arm64>` from `$NEXUS/install/` to
`/usr/local/bin/linexus-agent`, writes `/etc/linexus/agent.env` (0600:
`NEXUS_URL`, `ENROLLMENT_TOKEN`,
`AGENT_STATE_FILE=/var/lib/linexus/agent-state.json`), installs
[`deploy/linexus-agent.service`](deploy/linexus-agent.service) and starts it.
Re-running it upgrades the binary and keeps the agent's identity. Logs:
`journalctl -u linexus-agent -f`.

By hand: copy a binary to `/usr/local/bin/linexus-agent`, write
`/etc/linexus/agent.env`, copy the unit to `/etc/systemd/system/`, then
`systemctl daemon-reload && systemctl enable --now linexus-agent`.

## What it does

Nexus is the only service the agent talks to. It:

1. **Enrolls** once — `POST /api/v1/agents/enroll` with `{hostname,
   hostgroup, machineId, enrollmentToken}` and **no bearer** when
   `ENROLLMENT_TOKEN` is set (the legacy path sends no token in the body and
   `AGENT_TOKEN`, a Nexus system key, as the bearer). Nexus answers with the
   agent id and a per-agent `nxa_…` credential; both go into the state file
   and the credential is the bearer for every later call. `machineId`
   (`/etc/machine-id`, else `/var/lib/dbus/machine-id`) lets Nexus re-adopt a
   reinstalled host under its old agent id. Enrollment retries with backoff
   (up to every 5 min) until it succeeds.
2. **Reports facts** at start and every `AGENT_FACTS_INTERVAL` (see below).
3. **Heartbeats** for liveness.
4. **Polls** Nexus for tasks targeting it and **executes** each plan step.
5. **Ships logs and reports the result** back through Nexus (which relays logs to
   the Logger and updates task status). Daedalus IT then sees live health and the
   task's journal.

A `401` on any later call means the credential was revoked or rotated, or the
agent was deleted. The agent logs a loud, actionable `ERROR` (at most every
5 minutes, with a short line in between) and keeps heartbeating, so fixing it
on the Nexus side brings it back without a restart. To re-enroll: stop the
agent, remove the state file, put a fresh `ENROLLMENT_TOKEN` in
`/etc/linexus/agent.env`, start it.

### State file

`AGENT_STATE_FILE` (0600, written atomically):

```json
{
  "agentId": "0192…",
  "agentToken": "nxa_…",
  "enrollmentTokenId": "…",
  "hostgroup": "acme",
  "environment": {"environment": "production", "monitored": true, "note": "", "updatedAt": "…"}
}
```

Agents enrolled before per-agent credentials have only `agentId` and keep
using `AGENT_TOKEN` as their bearer.

### Facts

Each report carries the basics (`hostname, os, kernel, arch, cpuCores,
memoryMb, diskGb, agentVersion, uptimeSeconds`) plus, when available:

| Field | Source |
|---|---|
| `machineId` | `/etc/machine-id` |
| `publicIp` | first global-unicast IPv4 on an up, non-loopback interface that is not RFC 1918 or CGNAT (`100.64/10`); never an outbound lookup, so a NATed host reports none |
| `interfaces` | `[{name, mac, up, addresses: ["203.0.113.7/20", …]}]` |
| `listening` | `[{proto: tcp|udp, address, port, process}]` from `/proc/net/{tcp,tcp6,udp,udp6}` (TCP `LISTEN`, unconnected UDP); `process` from `/proc/*/fd` when readable |
| `services` | `[{name, state, detail, enabled, description}]` from `systemctl list-units --type=service --all` + `list-unit-files` (only under systemd) |
| `packages` | `[{name, version, manager: apt|dnf}]` from `dpkg-query` or `rpm -qa`, at most 5 000 |
| `dnsServer` | `{software: "bind9", version, running, zones}` when `named` is installed; `zones` are the ones this agent manages |

Every collector is read-only and best effort: one that is unavailable or
fails leaves its field out and never fails the report.

While an operator has marked the machine **not monitored**
(`agent.environment` with `monitored=false`), periodic facts reports stop
after the startup one; heartbeats and tasks continue, and an explicit
`agent.facts` (Refresh inventory) is still answered.

## Plan step vocabulary

The agent implements the action verbs the orchestrator emits
(`linexus-orch/src/plan.rs`):

| Action | Behavior |
|---|---|
| `command.run` | Runs `params.command` via `/bin/sh -c`, captures combined output |
| `package.ensure` | `name`, `state` (`present`/`absent`), `version` — apt or dnf; refreshes apt lists once when a package is unknown |
| `service.ensure` | `name`, `state` (`started`/`stopped`), `enabled` (`true`/`false`), `restart` (`true` restarts unconditionally and always reports changed) |
| `file.write` | `path`, `content`, `mode`, `owner`, `group` — atomic, by content hash |
| `agent.environment` | `environment` (default `production`), `monitored` (anything but `false`/`0`/`no` is true), `note` — persisted in the state file; identical assignments are unchanged |
| `agent.facts` | Collects and ships a facts report now |
| `dns.server.ensure` | Installs BIND (`bind9 bind9-utils` on apt, `bind bind-utils` on dnf), creates the zones directory and the agent's include file, appends one `include "…/linexus-zones.conf";` line to `named.conf.local` (Debian) / `named.conf` (RHEL), validates with `named-checkconf` (reverting its own line on failure), enables and starts `named`/`bind9`, and `rndc reconfig`s a server that was already running |
| `dns.zone.apply` | `zone`, `content`, `role` (`primary`/`secondary`), `primaries`, `secondaries`, `serial` — see below |
| `dns.zone.remove` | `zone` — drops the stanza, `rndc reconfig`, deletes `db.<zone>` (and its journal) |
| `disk.mount` | `device`, `mountPoint`, `fsType` (`ext4`/`xfs`), `format` (`if_blank`/`never`) — see below |
| `system.reboot` / `system.power_off` / `system.power_on` | **Suppressed by default** (dry-run). Set `AGENT_ALLOW_DESTRUCTIVE=1` to enable; power-on needs out-of-band control |
| `role.provision` | Acknowledged stub — real convergence (packages/services/files) is a later phase |
| anything else | Skipped with a note |

A failed **critical** step aborts the rest of the plan. Every mutating step
reads current state first and reports `changed` only when it acted.

### DNS (BIND)

The agent owns two things in BIND's configuration: an include file and a
zones directory.

| | Debian / Ubuntu | RHEL family | Override |
|---|---|---|---|
| main config (gains the include line) | `/etc/bind/named.conf.local` | `/etc/named.conf` | `AGENT_BIND_MAIN_CONF` (or `AGENT_BIND_CONF_DIR`) |
| include file | `/etc/bind/linexus-zones.conf` | `/etc/named/linexus-zones.conf` | `AGENT_BIND_INCLUDE` (or `AGENT_BIND_CONF_DIR`) |
| zones dir | `/var/lib/bind/linexus` (bind9's AppArmor profile allows writes there) | `/var/named/linexus` | `AGENT_BIND_ZONES_DIR` |
| service | `named`, or `bind9` where that is the real unit | `named` | `AGENT_BIND_SERVICE` |
| commands | `named-checkzone`, `named-checkconf`, `rndc` | same | `AGENT_BIND_CHECKZONE`, `AGENT_BIND_CHECKCONF`, `AGENT_BIND_RNDC` |

`dns.zone.apply` refuses to run until the main config
includes the agent's include file (run `install_dns_server` first), since a
reconfig would otherwise "succeed" and serve nothing. `dns.zone.apply`
validates the zone name strictly (letters, digits, hyphens,
dots; no slashes, no `..`), the addresses as IPs, and refuses `$INCLUDE`. For a
**primary** it stages `content` next to `<zones dir>/db.<zone>`, runs
`named-checkzone <zone> <staged file>` and only then renames it into place — a
zone that fails validation leaves the live file untouched and fails the step.
The stanza

```
zone "example.com" {
    type master;
    file "/var/lib/bind/linexus/db.example.com";
    allow-transfer { 10.0.0.9; };   // { none; } without secondaries
    also-notify { 10.0.0.9; };
};
```

(or `type slave; masters { <primaries>; };` for a **secondary**, which writes
no content) lives in a small registry, `<zones dir>/linexus-zones.json`, from
which the include file is re-rendered deterministically. A new or
re-configured zone gets `rndc reconfig`; an existing primary whose file
changed gets `rndc reload <zone>`. An unchanged file and stanza is
`unchanged` and reloads nothing (a secondary whose serial moved gets an
`rndc refresh`). A reload that fails marks the zone pending, and the next
apply retries it.

### Volumes

`disk.mount` waits up to 60 s for `device` to appear, probes it with
`blkid -p` and creates a filesystem (`mkfs.ext4 -F` / `mkfs.xfs`) **only** when
the device carries no signature at all and `format=if_blank` — a filesystem,
partition table or RAID/LVM signature is never touched (a device with a
different filesystem than asked is mounted as what it is). This does not need
`AGENT_ALLOW_DESTRUCTIVE`. It then creates the mount point, appends
`UUID=<uuid> <mountPoint> <fs> defaults,nofail,discard 0 2` to `/etc/fstab`
once (backing the original up to `/etc/fstab.linexus.bak`; a mount point or
volume already claimed by another line is refused), and mounts it unless
`/proc/self/mounts` already shows it there. System paths (`/`, `/etc`, `/usr`,
`/var`, `/boot`, …) are refused as mount points.

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
| `ENROLLMENT_TOKEN` | _(none)_ | One-time `nxe_…` token for the first enrollment; ignored once enrolled |
| `AGENT_TOKEN` | _(none)_ | Legacy: a Nexus system key, used to enroll when there is no `ENROLLMENT_TOKEN` and as the bearer for agents without a per-agent credential |
| `AGENT_HOSTGROUP` | _(none)_ | Enrollment hint, e.g. `web-prod` (an enrollment token's hostgroup wins) |
| `AGENT_STATE_FILE` | `linexus-agent-state.json` | Identity, credential and environment (the unit sets `/var/lib/linexus/agent-state.json`) |
| `AGENT_POLL_INTERVAL` | `10s` | Task poll cadence |
| `AGENT_HEARTBEAT_INTERVAL` | `30s` | Heartbeat cadence |
| `AGENT_FACTS_INTERVAL` | `5m` | Facts report cadence |
| `AGENT_BIND_*` | _(per distro)_ | BIND paths and commands, see [DNS](#dns-bind) |
| `AGENT_ALLOW_DESTRUCTIVE` | `false` | Permit reboot/power actions |
| `AGENT_ONCE` | `false` | Run a single cycle then exit (testing / one-shot) |

## Build & run

```sh
make                 # go vet + go test + ./rmm-agent for this host
make dist            # dist/rmm-agent-linux-amd64, dist/rmm-agent-linux-arm64, SHA256SUMS
ENROLLMENT_TOKEN=nxe_… NEXUS_URL=https://nexus.internal ./rmm-agent
```

Stdlib only — no external dependencies; binaries are static
(`CGO_ENABLED=0`). Put the `dist/` binaries in Nexus's
`LINEXUS_AGENT_BINARY_DIR` so `$NEXUS/install/rmm-agent-linux-<arch>` serves
them. `dist/` is git-ignored.
