# memgw pilot persistence — install, status, uninstall

Operational procedure for keeping this workstation's memgw **collectors** alive
across logoff and reboot.

> **The ledger moved. Read this first.**
>
> The canonical ledger is now the dedicated VPS cluster, not this workstation.
> The supervisor runs in **collectors-only** mode: it starts and watches the five
> collectors and does **not** start, require or restart a local gateway, and does
> not touch the local containers. The collectors forward to the authoritative
> gateway over TLS (`https://rag.seiz.cloud`), so the local stack has no role in
> the write path.
>
> Starting a local gateway would resurrect a **second writer** against a stopped
> local database. That is a rollback action requiring explicit authorisation, not
> an installer side effect — see `D:\memgw\run\cutover-state.txt`. Any rollback is
> **forward-only**: epoch 2 stays the only admitted epoch and the target stays
> authoritative.

**Scope.** This runbook covers process persistence only. It does not change
checkpoint semantics, the resolver, or any agent's settings. It starts the
same binaries with the same arguments an operator would type.

**Where the scripts live.** `ops/memgw/` in this repository.

| Script | Purpose |
|---|---|
| `Memgw.Common.ps1` | Shared: paths, secret reading, readiness probes, launch helpers, task hashing |
| `memgw-supervisor.ps1` | Starts the five collectors and restarts them on death. `-CollectorsOnly` is the registered mode; the default (`gateway+collectors`) is retained only for the documented rollback path |
| `install-memgw-persistence.ps1` | Registers `\memgw\supervisor` with `-CollectorsOnly` permanently; verifies by running it once **in the same mode it registered**. Requires only the five collector credentials — not the local database secret |
| `status-memgw-persistence.ps1` | Tasks, processes, gateway readiness, queue counts |
| `uninstall-memgw-persistence.ps1` | Removes exactly what install recorded |
| `install-memgw-ledger-pull.ps1` | Registers `\memgw\ledger-pull`: fetches the authoritative ledger backup from the VPS into the off-host copy and proves it recoverable |
| `uninstall-memgw-ledger-pull.ps1` | Removes exactly `\memgw\ledger-pull`, refusing if the task was hand-edited |

---

## 1. What gets installed

One Scheduled Task:

```
\memgw\supervisor
```

| Field | Value |
|---|---|
| Trigger | At logon of the installing user |
| Run as | Current user, `Interactive`, `Limited` (no elevation) |
| Action | `powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "…\memgw-supervisor.ps1"` |
| Working dir | `D:\memgw` |
| Settings | StartWhenAvailable, IgnoreNew, no execution time limit, restart 3× at 1-minute interval |

### Why one task, not three

The collectors share a spool layout and a set of credentials; one supervisor
owns starting and watching all five in one readable place, rather than three
independent tasks that could each be the one missing after a reboot.

### The backup tasks, and the two that were retired

The ledger's move changed which direction a backup travels. Two tasks were left
over from the old direction and have been **retired**, because both could only
ever move stale data:

| Task | Was | Now |
|---|---|---|
| `\memgw\ledger-backup` | Ran `D:\memgw\backup.ps1`, which dumps the **local** `memgw-live` container | **Retired.** That container no longer holds the ledger, so the script could only fail (`container memgw-live is not running`) or, worse, produce a well-formed artefact of a retired database. |
| `\memgw\ledger-offhost` | Pushed the newest local artefact to the VPS | **Retired.** It shipped a 136-event artefact of the retired workstation ledger while the authority was at 157 events, and exited 0 — every guard checked the artefact was *well-formed*, none asked whether it was still *the ledger*. The script now refuses when `memgw-live` is not running, but the task has no remaining job. |
| `\memgw\ledger-pull` | — | **New.** Fetches the authoritative artefact *from* the VPS into `D:\memgw-backups\vps\sealed`, verifies its sha256, and proves recovery by unsealing it with the escrowed key. A pull is the correct direction for an off-host copy: it survives a compromised or lost source host, which a push cannot. |

The gap this closed was real: `pull-memgw-backup.ps1` existed and was
documented, but **nothing scheduled it**. The only backup tasks that actually
ran were the two pushers, so the workstation held no off-host copy of the
authoritative ledger while appearing to have a working backup pipeline.

Install with `install-memgw-ledger-pull.ps1`; it verifies by performing one real
pull, which must exit 0 and produce a recovery proof in the log.

### Why Task Scheduler alone was not enough

Task Scheduler has no reliable restart-on-failure for a long-lived foreground
process — its `RestartCount`/`RestartInterval` apply to *task* failures, not to
a process that exits later while the task is still nominally running. The
supervisor script is that missing piece: it polls every 5 s and restarts
anything that died.

---

## 2. Secrets

**No secret appears in any task action, argument, log, or process command line.**

- Each collector names its credential with `--token-file <path>`; the binary
  reads the file itself. The value never enters this toolkit's process.
- **There is no local database secret any more.** The collectors-only install
  does not compose a `MEMGW_DSN`, so it does not read `D:\memgw\secrets\pg_app`
  at all, and it cannot fail on a machine whose local ledger has been retired.
  That dependency is gone deliberately: it was what made a client install fail
  after the authority moved. Proven at runtime by moving `pg_app` aside and
  running the collectors-only one-shot to a successful exit.

The gateway path below applies **only** to the documented rollback scenario.
When it is used, the gateway's PostgreSQL password is read from
`D:\memgw\secrets\pg_app` and composed into `MEMGW_DSN` **inside the PowerShell
runspace**, then handed to the child through its environment block — never as an
argument, and logs record the DSN **redacted**, never the credential. Verified:
launching the gateway through the helper produces the command line

```
"D:\memgw\bin\enowx-rag.exe" memgw serve --addr 127.0.0.1:7791
```

with no password in it.

---

## 3. Install

```powershell
cd D:\PROJECTS\enowx-rag\ops\memgw
powershell -NoProfile -ExecutionPolicy Bypass -File .\install-memgw-persistence.ps1
```

Dry run first:

```powershell
.\install-memgw-persistence.ps1 -WhatIf
```

What it does, in order:

1. Checks the binary and the **five per-host collector credentials** exist
   (`claude`, `omp`, `codex`, `droid`, `opencode` `.token`). It does **not**
   require `pg_app`: this install starts no local gateway, so it must not fail
   on a machine whose local ledger has been retired.
2. **Refuses** if a task named `\memgw\supervisor` already exists.
3. Records the pre-existing task state to `D:\memgw\backups\ops-<stamp>\`.
4. Registers the task.
5. Writes the install manifest to `D:\memgw\run\installed-tasks.json`,
   including a hash of the task action.
6. **Verifies** by running the supervisor once in **the same mode it
   registered** (`-CollectorsOnly -Once`), confirming every collector pipe
   opens. The install fails loudly if that verification fails.

The verification runs the same script, interpreter **and mode flag** the task
will use, so it exercises the real path rather than describing it. This matters:
an earlier version verified with `-Once` but *without* `-CollectorsOnly`, so it
exercised the gateway+collectors path and would have passed while the installed
collectors-only task did something else entirely.

### Agent hook timeouts are NOT installed by this script

`install-memgw-persistence.ps1` installs the **supervisor task only**. The agent
hook wiring (Claude Code `settings.json`, OMP/OpenCode extensions, Codex and
Droid `hooks.json`) is configured separately, and one setting in it is load
bearing:

The `--capture-turn` hooks must pass an explicit `--timeout` well above the
binary's internal `-timeout` default of **15 s**. The child model turn that
authors a checkpoint takes 60–80 s, so a hook relying on the default aborts
every capture and model-authored checkpoints become **silently impossible** —
the wrapper looks generous while the inner default fires first. The installed
Claude Code hooks use `--timeout 150s` with a 180 s wrapper. If the hook config
is ever regenerated, re-apply that or captures will stop being written without
any error surfacing in the session.

---

## 4. Status

```powershell
.\status-memgw-persistence.ps1           # human-readable
.\status-memgw-persistence.ps1 -Json     # machine-readable
```

Reports four independent things, because they fail independently:

1. **Tasks** — registered? state? last run? last result?
2. **Processes** — gateway and collectors, by pid and start time.
3. **Gateway readiness** — an HTTP probe, not a pid check. A process can be
   alive and not serving.
4. **Queue counts** — `pending`, `sending`, `sent`, `quarantined`, `dead` per
   collector spool. Warns when `quarantined` or `dead` is non-zero.

### Reading the readiness probe

The gateway requires a per-principal credential, so an unauthenticated request
gets **401**. That refusal is proof the listener is bound and routing, and it
needs no credential to learn — which is exactly what a readiness probe wants.
The probe therefore treats *any* HTTP response as ready and only "connection
refused / no response" as not ready.

---

## 5. Uninstall

```powershell
.\uninstall-memgw-persistence.ps1              # remove tasks, leave processes
.\uninstall-memgw-persistence.ps1 -StopProcesses
.\uninstall-memgw-persistence.ps1 -WhatIf      # dry run
```

Reversal is exact and conservative:

- Reads `D:\memgw\run\installed-tasks.json`.
- **Refuses** if the task's action hash no longer matches the manifest —
  someone edited the task by hand, so it is not ours to delete.
- Writes the stop file, then **waits for the supervisor process to disappear**
  (up to 30 s) rather than sleeping a fixed interval, so the marker is not
  deleted before it is read.
- Unregisters the task and removes the now-empty `\memgw\` task folder.
- Removes the stop file only once the supervisor is confirmed gone; otherwise
  it warns and leaves the marker so the supervisor exits at its next poll.
- Touches **no** spool, log, secret, database, or binary.

---

## 6. Verifying without rebooting

A reboot is not required to prove the tasks work:

```powershell
# 1. Stop only the pilot processes.
powershell -NoProfile -Command ". .\Memgw.Common.ps1; Get-MemgwRunningProcesses | ForEach-Object { Stop-Process -Id $_.ProcId -Force }"

# 2. Run the real task.
Start-ScheduledTask -TaskPath '\memgw\' -TaskName 'supervisor'

# 3. Confirm.
.\status-memgw-persistence.ps1
```

`Get-MemgwRunningProcesses` matches on the **resolved binary path** plus the
`memgw serve|collector` subcommand, so it never returns an unrelated process
that merely mentions "memgw".

To prove restart-on-failure, kill one collector and watch the supervisor bring
it back:

```powershell
. .\Memgw.Common.ps1
$c = Get-MemgwRunningProcesses | Where-Object { $_.Name -eq 'omp' }
Stop-Process -Id $c.ProcId -Force
Start-Sleep 15
Get-MemgwRunningProcesses     # omp is back with a new pid
```

The `omp.log` will show:

```
[warn] collector is gone; restarting
[info] collector started pid=… pipe=\\.\pipe\memgw-collector-omp …
```

---

## 7. Stopping the supervisor without uninstalling

```powershell
New-Item -Path D:\memgw\run\supervisor.stop -ItemType File -Force
```

The supervisor exits at its next poll and **leaves the child processes
running** — they are independent processes, not job-object children. Remove the
file before the next logon, or the freshly started task will stop immediately.
(The install script removes a stale stop file for this reason.)

---

## 8. Logs

| Path | Contents |
|---|---|
| `D:\memgw\logs\ops.log` | Install/uninstall/supervisor lifecycle |
| `D:\memgw\logs\gateway\gateway.log` | Gateway start, redacted DSN, readiness |
| `D:\memgw\logs\collectors\<name>.log` | Collector start, pipe, spool, token-**path**, restarts |

All on `D:`. The log tree has inherited ACEs removed at install (Administrators,
SYSTEM, and the current user only).

---

## 9. Known limits

- **Logon-triggered, not boot-triggered.** It runs when the user logs in. It
  does not run at boot before any user session, because the spool, the token
  files and the agent sessions all belong to that user. A boot-time service
  would need a different identity and its own credentials.
- **No log rotation.** The logs are append-only. They are small (one line per
  event), but a supervisor that restarts a crashing collector every 5 s will
  grow `ops.log` steadily. Watch it if a collector ever crash-loops.
- **`D:` must be present at logon.** The spools, logs, secrets and binary all
  live there. If `D:` is a volume that mounts late, the first run may fail;
  `StartWhenAvailable` plus the supervisor's own retry covers a late mount
  within the task's restart window.
- **Secrets inherit broad ACLs.** `D:\memgw\secrets\*` currently inherits
  `Authenticated Users:(M)` from `D:`. The toolkit does not change this
  (it does not own those files), but it is worth tightening to the installing
  user alone. Reported, not silently fixed.

---

## 10. Evidence from the pilot install

Recorded 2026-09-10 on this workstation.

| Check | Result |
|---|---|
| Task registered | `\memgw\supervisor`, state `Ready` |
| Task action contains a secret | no |
| Gateway launched via helper — password in command line | no |
| Supervisor one-shot: gateway ready | yes |
| Supervisor one-shot: both collector pipes open | yes |
| Kill all 3 pilot processes → run task → stack restored | yes (new pids, ready) |
| Kill 1 collector → supervisor restores it within one poll | yes |
| Uninstall removes task + empty folder | yes |
| Uninstall touches spools/secrets/database | no |
| Duplicate supervisor instances after re-run | none (1) |
