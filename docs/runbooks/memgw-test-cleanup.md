# Test resource cleanup — staged, prepared, not executed

> **Nothing here has been run.** No container was stopped, no credential
> revoked, no file deleted. The disposable resources listed in the main plan §8
> are all still alive. This sheet exists so that removing them is a controlled
> act with a verifiable result at each stage, rather than one `rm -rf` that also
> takes something it should not have.

Cleanup needs the user's explicit approval (gate item 9). It is listed last on
purpose: these resources hold the only evidence behind several `verified` rows,
and two of them are live test credentials that must be revoked **at the ledger**
before their files are deleted.

## Rules that apply to every stage

- **Never a global prune.** Not `docker system prune`, not `docker volume
  prune`, not `docker container prune`. This machine holds containers and
  volumes belonging to other work. Act on names, one at a time.
- **Never touch** `seizrag-qdrant`, any `sc_recovery_*` volume, `D:\ClaudeVM`,
  or the `vm_bundles` junction that points at it.
- **Do not open a credential file.** Not to check it, not to confirm it is the
  right one. Its path is enough; its contents are not needed for anything in
  this procedure.
- Verify each stage before starting the next. If a stage's expectation is not
  met, stop there — a half-cleaned spool with a live credential is worse than
  the untouched state.

---

## Stage 1 — revoke the test credentials at the ledger

**This is first, not fourth.** A credential deleted from the filesystem is not
revoked; it is only harder to find. The database is where authority lives, and
`memgw-devdb` has to still be running for this stage, which is why the container
is stopped in stage 3 rather than before.

Two test principals hold live credentials:

| Principal | Id | Purpose |
|---|---|---|
| `claude-code` | `159fdcc6-8f08-4cb9-b57b-f62d6b17db69` | collector test credential |
| `rehearsal-admin` | `a72ac30d-45dd-4bbc-b389-ec02ab5d7635` | credential `0c1e1632-9685-4491-adf4-b3a4e46dc222` |

A third, `64345493-bc90-4ede-9f94-13a53ad855bb`, was leaked to a transcript
earlier in this work and is **already revoked**. It is listed so the revocation
is not repeated or forgotten, not because it needs action.

```powershell
$env:MEMGW_DSN    = 'postgres://postgres:memgw@127.0.0.1:55433/memgw_test?sslmode=disable'
$env:MEMGW_SCHEMA = 'memgw'
$env:MEMGW_ENV    = 'test'

# read the credential ids first; this prints metadata only, never a secret
D:\memgw-test\bin\memgw-smoke.exe memgw principal list 159fdcc6-8f08-4cb9-b57b-f62d6b17db69
D:\memgw-test\bin\memgw-smoke.exe memgw principal list a72ac30d-45dd-4bbc-b389-ec02ab5d7635

# revoke each credential id the listing showed as not yet revoked
D:\memgw-test\bin\memgw-smoke.exe memgw principal revoke-credential <CREDENTIAL_ID>
D:\memgw-test\bin\memgw-smoke.exe memgw principal revoke-credential 0c1e1632-9685-4491-adf4-b3a4e46dc222
```

**Expected**: re-running `principal list` shows a `revoked` timestamp on every
credential row for both principals.

Credential rows cannot be deleted — a trigger prevents it. Revocation is the
only undo, and it is sufficient.

---

## Stage 2 — confirm nothing is using the resources

```powershell
Get-Process enowx-rag,memgw-smoke,memgw-graphify,memgw-history,memgw-shadow `
            -ErrorAction SilentlyContinue
Get-NetTCPConnection -LocalPort 7777 -State Listen -ErrorAction SilentlyContinue
Get-ChildItem \\.\pipe\ | Where-Object Name -like 'memgw-*'
```

**Expected**: all three print nothing. As of the last inventory that was already
true — no memgw process was running and nothing listened on `127.0.0.1:7777`.

If a process is found, stop it and re-check. Do not proceed while a collector
holds the spool: on Windows the delete in stage 4 will simply fail, and it will
leave a WAL sidecar behind.

---

## Stage 3 — stop the disposable containers, by name

```powershell
docker stop memgw-devdb
docker stop memgw-qdrant
docker rm memgw-devdb
docker rm memgw-qdrant
docker ps -a --filter name=memgw-
```

**Expected**: the final listing is empty.

Only these two names. The other containers on this machine
(`reminder_postgres`, `reminder_redis`, and the protected Qdrant) belong to
other work and are not part of this cleanup.

This destroys `memgw_test`, `memgw_itest` and `memgw_restore` along with the
container, including the ledger debris — project
`7627423a-fd6e-459d-87c0-be32cd47c8cb`, both test principals and the writer
epochs. That is intended, and it is why stage 1 comes first.

---

## Stage 4 — verify no open handles, then delete the spools and keys

```powershell
Get-ChildItem D:\memgw-test -Recurse -Filter 'spool.db*'
```

**Expected**: the `.db` files, and no `-wal` or `-shm` sidecar left behind by a
live process. A sidecar means stage 2 was not really satisfied.

Then, and only after stage 1 reported every credential revoked:

```powershell
Remove-Item D:\memgw-test\collector\token
Remove-Item D:\memgw-test\rehearsal\token
Remove-Item D:\memgw-test\collector\spool.key
Remove-Item D:\memgw-test\adapter-collector\spool.key
```

The two `spool.key` files are DPAPI-wrapped under this user's profile. Deleting
them makes the spools permanently unreadable, which is the point — but it also
means an undeleted spool afterwards is undecryptable rather than merely stale.
Delete the keys and the spools together, not the keys alone.

---

## Stage 5 — remove the remaining test data

```powershell
Remove-Item -Recurse -Force D:\memgw-test\collector
Remove-Item -Recurse -Force D:\memgw-test\adapter-collector
Remove-Item -Recurse -Force D:\memgw-test\rehearsal
Remove-Item -Recurse -Force D:\memgw-test\backup
Remove-Item -Recurse -Force D:\memgw-test\history
Remove-Item -Recurse -Force D:\memgw-test\graphify
Remove-Item -Recurse -Force D:\memgw-test\adapter
Remove-Item -Recurse -Force D:\memgw-test\bin
Remove-Item -Recurse -Force D:\memgw-test\qdrant-storage
Remove-Item D:\memgw-test\gstatus.json, D:\memgw-test\serve.env.sh
Remove-Item D:\memgw-test\*.out, D:\memgw-test\*.err
Get-ChildItem D:\memgw-test
```

**Expected**: the final listing is empty, at which point `D:\memgw-test` itself
can go.

`serve.env.sh` holds the disposable container's password and a loopback DSN, and
nothing else. The `*.out` / `*.err` logs hold no credential: the collector reads
its token from a file and never logs it.

`qdrant-storage` is a bind mount for the **test** Qdrant only. It is not a
production volume and it is not `sc_recovery_*`.

---

## Stage 6 — verify no production path was touched

```powershell
docker ps --filter name=seizrag-qdrant           # still running, still that name
docker volume ls --filter name=sc_recovery_      # every volume still listed
Test-Path D:\ClaudeVM                            # True
Get-Item D:\PROJECTS\*\vm_bundles -ErrorAction SilentlyContinue
```

**Expected**: the protected Qdrant container is running under its original name;
every `sc_recovery_*` volume is present; `D:\ClaudeVM` exists and the
`vm_bundles` junction still resolves to it.

Also true, and worth stating rather than assuming: nothing in this cleanup
contacts the production host, reads `.env`, touches nginx or systemd, or opens a
credential file.

---

## What cleanup costs

Removing these resources removes the evidence behind readiness rows 10, 15 and
16, and behind every runtime claim in plan §5.3–§5.9. The plan records the
results, but the artifacts that produced them will be gone and the runs are not
cheap to repeat.

Do this **after** the package is accepted, not before — and not at all while any
row in §6 might still need re-proving.
