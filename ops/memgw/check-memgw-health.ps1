#Requires -Version 5.1
<#
.SYNOPSIS
    One-shot health check for the memgw gateway, its collectors and the remote
    enowx-rag service. Write a status line and alert on threshold breaches.

.DESCRIPTION
    The cutover runbook names this as readiness row 18: "There is no alerting
    on any of this. Someone has to look." This script is the thing that looks.

    It checks, in order:

      local gateway     the HTTP listener answers on 127.0.0.1:7791 (any HTTP
                        status proves the listener is up; a 401 is normal
                        because the gateway authenticates per principal)
      collectors         the supervised enowx-rag processes are alive and each
                        collector's named pipe exists
      queue age          the ledger's oldest undelivered projection_outbox row
                        (pending/failed/claimed) -- a growing age while the
                        gateway is up means the projection worker is not
                        draining
      dead letters       projection_outbox rows in state 'dead' (retries
                        exhausted; these never move on their own)
      projection lag     max(events.seq) - projection_watermark.applied_seq
      remote service     https://rag.seiz.cloud/api/stats returns 401 without a
                        token (proves the listener is up and auth is not failing
                        open); a 200 without a token is an incident
      off-host backup    a sealed `axonhub` artefact in the pull's own
                        destination, at a realistic size, with a manifest that
                        agrees with the file
      backup pipeline    the VPS staging unit's result and its timer's state
      ledger backup      a sealed dump of the canonical ledger (container
                        memgw-live) whose manifest records a real ledger
      ledger off-host    the same artefact present on the VPS, root-only, kept
                        fresh

    Two of these exist because an earlier revision got them wrong. The backup
    check used to accept any sealed file by mtime, so four 114 KB dumps from a
    scratch database reported `ok` while the real target failed every run; and
    the ledger had no backup at all. A control that reports green over a broken
    pipeline is worse than no control, so both now verify identity, size and
    provenance rather than presence.

    Every check records a row; the script exits non-zero when any check crosses
    its threshold, so a Scheduled Task can be failed visibly rather than only
    logging.

    Secrets: no credential is read. The remote probe deliberately sends no
    token. The database is reached through the local running container as the
    postgres superuser over its unix socket, matching how status-memgw-
    persistence.ps1 already inspects state; no DSN is composed here.

.NOTES
    Read-only. Touches no canonical state, no repository source, and never the
    protected seizrag-qdrant container.
#>

[CmdletBinding()]
param(
    # Warn when the oldest undelivered outbox row is older than this.
    [int]$QueueAgeWarnSeconds = 600,
    # Fail when the oldest undelivered outbox row is older than this.
    [int]$QueueAgeFailSeconds = 3600,
    # Warn when projection lag (events - applied) exceeds this.
    [int]$LagWarn = 500,
    # Fail when projection lag exceeds this.
    [int]$LagFail = 5000,
    # Emit the report as JSON instead of text.
    [switch]$Json
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Continue'

$statusDir = 'D:\memgw\logs'
$statusFile = Join-Path $statusDir 'health-status.json'
New-Item -ItemType Directory -Force -Path $statusDir | Out-Null

$checks = New-Object System.Collections.Generic.List[object]

function Add-Check {
    param([string]$Name, [string]$State, [string]$Detail)
    $checks.Add([pscustomobject]@{ check = $Name; state = $State; detail = $Detail })
}

# --- local gateway ---------------------------------------------------------
try {
    $resp = Invoke-WebRequest -Uri 'http://127.0.0.1:7791/' -UseBasicParsing -TimeoutSec 5 -ErrorAction Stop
    Add-Check 'gateway-listener' 'ok' "answered HTTP $($resp.StatusCode)"
} catch {
    # A 401/404 is still an answer; only a transport failure is a failure.
    if ($_.Exception.Response) {
        Add-Check 'gateway-listener' 'ok' "answered HTTP $([int]$_.Exception.Response.StatusCode)"
    } else {
        Add-Check 'gateway-listener' 'fail' "no answer on 127.0.0.1:7791: $($_.Exception.Message)"
    }
}

# --- collector processes ---------------------------------------------------
$procs = @(Get-Process enowx-rag -ErrorAction SilentlyContinue)
if ($procs.Count -ge 3) {
    Add-Check 'collector-processes' 'ok' "$($procs.Count) enowx-rag processes running (gateway + 2 collectors)"
} elseif ($procs.Count -ge 1) {
    Add-Check 'collector-processes' 'warn' "only $($procs.Count) enowx-rag process(es) running; expected >= 3"
} else {
    Add-Check 'collector-processes' 'fail' 'no enowx-rag processes running'
}

# --- ledger queue / dead letters / projection lag --------------------------
# Read through the running container; the query is read-only and touches no
# credential.
$sql = @"
SELECT
  COALESCE((SELECT count(*) FROM memgw.projection_outbox WHERE state IN ('pending','claimed','failed')), 0) AS undelivered,
  COALESCE((SELECT count(*) FROM memgw.projection_outbox WHERE state = 'dead'), 0) AS dead_letters,
  COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at)))::int FROM memgw.projection_outbox WHERE state IN ('pending','claimed','failed')), 0) AS oldest_age_s,
  COALESCE((SELECT max(seq) FROM memgw.events), 0) AS max_seq,
  COALESCE((SELECT max(applied_seq) FROM memgw.projection_watermark WHERE projection = 'qdrant'), 0) AS applied_seq;
"@ -replace "`r?`n", ' '

$row = $null
try {
    $out = docker exec -u postgres memgw-live psql -U postgres -d memgw -tA -F '|' -c $sql 2>$null
    if ($out) {
        $parts = ($out | Select-Object -Last 1) -split '\|'
        $row = [pscustomobject]@{
            undelivered = [int]$parts[0]
            dead_letters = [int]$parts[1]
            oldest_age_s = [int]$parts[2]
            max_seq = [long]$parts[3]
            applied_seq = [long]$parts[4]
        }
    }
} catch { }

if ($null -eq $row) {
    Add-Check 'ledger-query' 'fail' 'could not read the ledger (container memgw-live unreachable or query failed)'
} else {
    $lag = [long]($row.max_seq - $row.applied_seq)

    if ($row.dead_letters -gt 0) {
        Add-Check 'dead-letters' 'fail' "$($row.dead_letters) projection_outbox rows in state 'dead' (never move on their own)"
    } else {
        Add-Check 'dead-letters' 'ok' 'no dead letters'
    }

    if ($row.oldest_age_s -ge $QueueAgeFailSeconds) {
        Add-Check 'queue-age' 'fail' "oldest undelivered row is $($row.oldest_age_s)s old (>= ${QueueAgeFailSeconds}s)"
    } elseif ($row.oldest_age_s -ge $QueueAgeWarnSeconds) {
        Add-Check 'queue-age' 'warn' "oldest undelivered row is $($row.oldest_age_s)s old (>= ${QueueAgeWarnSeconds}s)"
    } else {
        Add-Check 'queue-age' 'ok' "$($row.undelivered) undelivered, oldest $($row.oldest_age_s)s"
    }

    if ($lag -ge $LagFail) {
        Add-Check 'projection-lag' 'fail' "lag $lag events (max seq $($row.max_seq), applied $($row.applied_seq))"
    } elseif ($lag -ge $LagWarn) {
        Add-Check 'projection-lag' 'warn' "lag $lag events (max seq $($row.max_seq), applied $($row.applied_seq))"
    } else {
        Add-Check 'projection-lag' 'ok' "lag $lag events (max seq $($row.max_seq), applied $($row.applied_seq))"
    }
}

# --- remote enowx-rag service ----------------------------------------------
# No token: a 401 is the correct, cheapest proof the service is up and that
# auth is not failing open.
try {
    $code = (Invoke-WebRequest -Uri 'https://rag.seiz.cloud/api/stats' -UseBasicParsing -TimeoutSec 10 -ErrorAction Stop).StatusCode
    # 200 without a token is an incident.
    Add-Check 'remote-service' 'fail' "public /api/stats returned $code WITHOUT a token (auth is failing open)"
} catch {
    if ($_.Exception.Response) {
        $code = [int]$_.Exception.Response.StatusCode
        if ($code -eq 401) {
            Add-Check 'remote-service' 'ok' 'public /api/stats answered 401 without a token (expected)'
        } else {
            Add-Check 'remote-service' 'warn' "public /api/stats answered $code without a token (expected 401)"
        }
    } else {
        Add-Check 'remote-service' 'fail' "no answer from rag.seiz.cloud: $($_.Exception.Message)"
    }
}

# --- off-host backup of the production database (axonhub) -------------------
# This check used to look at the newest `*.aes` by age alone, which made it the
# most dangerous kind of control: during a manual test it found four
# 114 KB `travelya` dumps from a 10 MB scratch database, saw a fresh mtime, and
# reported `ok` -- while the real target (`axonhub`, 8.9 GB) had failed every
# scheduled run. A green light over a broken pipeline is worse than no light.
#
# So the check now reads the manifest each sealed artefact carries, and counts
# only the production database at a realistic size. Test artefacts cannot pass
# no matter how fresh they are.
# Only the scheduled pull's destination counts: `vps\sealed`. A full-size
# axonhub dump also sits at the root from an earlier one-shot manual transfer,
# and counting it was worse than useless -- it made the check green while the
# pipeline it is supposed to monitor had never once succeeded. A production
# backup check must observe the pipeline, not the leftovers.
$sealedDirs = @('D:\memgw-backups\vps\sealed')
$prodDatabase = 'axonhub'
# The axonhub dump compresses to ~4.7 GB. A gigabyte floor is far above any
# scratch database (travelya is ~114 KB) and far below the real thing, so it
# separates "a real production dump" from "something else" with no tuning.
$prodMinBytes = 1GB
$backupWarn = [TimeSpan]::FromHours(30)
$backupFail = [TimeSpan]::FromHours(72)

$artefacts = @($sealedDirs | ForEach-Object {
    Get-ChildItem -LiteralPath $_ -Filter '*.aes' -File -ErrorAction SilentlyContinue
})
$prodCandidates = @()
$sawOtherDb = New-Object System.Collections.Generic.List[string]
foreach ($a in $artefacts) {
    # Database identity comes from the manifest when the pull wrote one, and
    # from the filename prefix otherwise (the manual transfer wrote none).
    $m = Get-Item -LiteralPath "$($a.FullName).manifest" -ErrorAction SilentlyContinue
    $fields = @{}
    if ($m) {
        foreach ($line in (Get-Content -LiteralPath $m.FullName -ErrorAction SilentlyContinue)) {
            $i = $line.IndexOf('=')
            if ($i -gt 0) { $fields[$line.Substring(0, $i).Trim()] = $line.Substring($i + 1).Trim() }
        }
    }
    $dbName = if ($fields.ContainsKey('database')) { $fields['database'] } else { ($a.Name -split '-')[0] }
    if ($dbName -ne $prodDatabase) {
        $sawOtherDb.Add($dbName)
        continue
    }
    $len = $a.Length
    if ($len -lt $prodMinBytes) { continue }
    $prodCandidates += [pscustomobject]@{
        Artefact = $a.FullName
        Name     = $a.Name
        Bytes    = $len
        ManifestBytes = [long]$fields['bytes']
        TakenAt  = $fields['taken_at']
        WriteTime = $a.LastWriteTime
    }
}

if ($prodCandidates.Count -eq 0) {
    $other = if ($sawOtherDb.Count -gt 0) { " (only non-production: $(($sawOtherDb | Sort-Object -Unique) -join ', '))" } else { '' }
    Add-Check 'offhost-backup' 'fail' "no sealed $prodDatabase backup >= $([int]($prodMinBytes / 1MB))MB in $($sealedDirs -join ', ')$other"
} else {
    $newest = $prodCandidates | Sort-Object WriteTime -Descending | Select-Object -First 1
    $age = (Get-Date) - $newest.WriteTime
    # Provenance must agree with the file it names. The manifest records the
    # size of the staged `.dump.gz` *before* sealing, and AES-CBC/PKCS7 adds
    # 1-16 bytes of padding, so an exact match is the wrong test -- a sealed
    # file may legitimately be up to 16 bytes larger. A mismatch beyond that
    # window is a copy/attribution bug, not a stale backup.
    $sizeAgrees = ($newest.ManifestBytes -le 0) -or
                  ($newest.Bytes -ge $newest.ManifestBytes -and $newest.Bytes -le ($newest.ManifestBytes + 16))
    $desc = "$($newest.Name) ($([math]::Round($newest.Bytes / 1GB, 2))GB, $([int]$age.TotalHours)h old)"
    if (-not $sizeAgrees) {
        Add-Check 'offhost-backup' 'fail' "$desc but its manifest claims $($newest.ManifestBytes) bytes"
    } elseif ($age -ge $backupFail) {
        Add-Check 'offhost-backup' 'fail' "newest sealed $prodDatabase backup is $([int]$age.TotalHours)h old ($desc)"
    } elseif ($age -ge $backupWarn) {
        Add-Check 'offhost-backup' 'warn' "newest sealed $prodDatabase backup is $([int]$age.TotalHours)h old ($desc)"
    } else {
        Add-Check 'offhost-backup' 'ok' "$desc"
    }
}

# --- VPS backup pipeline state ----------------------------------------------
# Freshness alone only notices a broken pipeline after the freshness window
# (72h). Asking systemd directly notices it the same day: a `failed` unit or a
# timer that is gone means no new dump is coming, whatever is on disk today.
# An SSH failure here is a `warn`, not a `fail`: not being able to ask is a
# different fact from the answer being bad, and the freshness check above
# still fails if the pipeline really has stopped.
$vpsTimer = 'rag-pg-backup.timer'
$vpsUnit  = 'rag-pg-backup.service'
$sshArgs = @('-i', "$env:USERPROFILE\.ssh\id_oracle", '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10',
             'ubuntu@168.110.218.207')
$remote = & ssh @sshArgs "systemctl is-active $vpsTimer; systemctl is-failed $vpsUnit; systemctl show $vpsUnit -p Result -p ExecMainStatus" 2>&1
if ($LASTEXITCODE -ne 0 -and $remote -notmatch '^(active|inactive|failed)') {
    Add-Check 'backup-pipeline' 'warn' "could not query $vpsUnit over ssh: $($remote | Select-Object -First 1)"
} else {
    $timerState = "$($remote[0])".Trim()
    $unitFailed = "$($remote[1])".Trim()
    $resultLine = ($remote | Where-Object { $_ -match '^ExecMainStatus=' }) -join ' '
    $resultValue = ($remote | Where-Object { $_ -match '^Result=' }) -replace '^Result=', ''
    $status = ($resultValue | Select-Object -First 1)
    if ($timerState -ne 'active') {
        Add-Check 'backup-pipeline' 'fail' "$vpsTimer is '$timerState'; the daily staging run will not fire"
    } elseif ($unitFailed -eq 'failed') {
        Add-Check 'backup-pipeline' 'fail' "$vpsUnit last result $status ($resultLine); no new dump is being staged"
    } else {
        Add-Check 'backup-pipeline' 'ok' "$vpsTimer active; $vpsUnit last result $status ($resultLine)"
    }
}

# --- canonical ledger backup (container memgw-live) -------------------------
# The ledger is the authority for every agent's work state. Until recently it
# had no backup at all, and the one artefact that existed was a 1,270-byte
# text-mode redirect of a binary dump: it had the right shape and restored
# nothing. The task now runs daily; this check confirms a sealed copy exists
# and that its manifest records a real ledger (the TOC-entry count is what the
# old broken dump failed).
$ledgerDir = 'D:\memgw-backups'
$ledgerWarn = [TimeSpan]::FromHours(36)
$ledgerFail = [TimeSpan]::FromHours(96)
$newestLedger = Get-ChildItem -LiteralPath $ledgerDir -Filter '*.dump.dpapi' -ErrorAction SilentlyContinue |
    Sort-Object LastWriteTime -Descending | Select-Object -First 1
if ($null -eq $newestLedger) {
    Add-Check 'ledger-backup' 'fail' "no sealed ledger backup in $ledgerDir"
} else {
    $age = (Get-Date) - $newestLedger.LastWriteTime
    $mPath = "$($newestLedger.FullName).manifest"
    $toc = $null
    if (Test-Path -LiteralPath $mPath) {
        $mLine = Get-Content -LiteralPath $mPath | Where-Object { $_ -match '^toc_entries=' }
        if ($mLine) { $toc = [int]($mLine -replace '^toc_entries=', '') }
    }
    $desc = "$($newestLedger.Name) ($([math]::Round($newestLedger.Length / 1KB))KB, $([int]$age.TotalHours)h old)"
    if ($null -eq $toc) {
        Add-Check 'ledger-backup' 'fail' "$desc but no readable manifest beside it"
    } elseif ($toc -lt 100) {
        Add-Check 'ledger-backup' 'fail' "$desc but its manifest records only $toc TOC entries (an empty or text-mangled dump)"
    } elseif ($age -ge $ledgerFail) {
        Add-Check 'ledger-backup' 'fail' "newest sealed ledger backup is $([int]$age.TotalHours)h old ($desc)"
    } elseif ($age -ge $ledgerWarn) {
        Add-Check 'ledger-backup' 'warn' "newest sealed ledger backup is $([int]$age.TotalHours)h old ($desc)"
    } else {
        Add-Check 'ledger-backup' 'ok' "$desc, $toc TOC entries"
    }
}

# --- off-host ledger copy (different failure domain) ------------------------
# The sealed ledger backup above lives on D:, the same workstation as the
# ledger. A second volume is not a second failure domain. This checks that the
# copy on the VPS exists and records a plausible ledger (TOC >= 100), so a
# push that silently stopped is noticed before the workstation is lost rather
# than after. An SSH failure is a `warn`: not being able to ask is a different
# fact from the answer being bad, and the local check above still applies.
$offhostDir = '/opt/rag-backup/memgw-ledger-offhost'
$offhostWarn = [TimeSpan]::FromHours(36)
$offhostFail = [TimeSpan]::FromHours(96)
# One remote script over stdin: `ssh host 'sudo bash -s'` has no quoting to get
# wrong, and the globbing happens remotely so nothing local has to guess the
# newest name.
$offhostScript = @"
set -u
newest=`$(ls -1t $offhostDir/memgw-*.dump.dpapi 2>/dev/null | head -n1)
if [ -z "`$newest" ]; then echo "EMPTY"; exit 0; fi
echo "name=`$(basename "`$newest")"
stat -c 'stat=%a %U:%G' "`$newest"
grep -h '^toc_entries=' "`$newest.manifest" 2>/dev/null || echo "toc_entries=missing"
"@
$offhostOut = $offhostScript | & ssh @sshArgs 'sudo bash -s' 2>&1
$offhostLines = @($offhostOut | ForEach-Object { "$_".Trim() } | Where-Object { $_ })
if ($LASTEXITCODE -ne 0 -and $offhostLines.Count -eq 0) {
    Add-Check 'ledger-offhost' 'warn' "could not query the VPS copy: $(($offhostLines | Select-Object -First 1))"
} elseif ($offhostLines.Count -eq 0) {
    Add-Check 'ledger-offhost' 'fail' "no sealed ledger copy in ${offhostDir} on the VPS"
} else {
    $nameLine = ($offhostLines | Where-Object { $_ -match '^name=' } | Select-Object -First 1)
    $name = if ($nameLine) { $nameLine -replace '^name=', '' } else { '(unnamed)' }
    $statLine = ($offhostLines | Where-Object { $_ -match '^stat=' } | Select-Object -First 1)
    $tocLine = ($offhostLines | Where-Object { $_ -match '^toc_entries=\d+' } | Select-Object -First 1)
    $remoteToc = if ($tocLine) { [int]($tocLine -replace '^toc_entries=', '') } else { $null }
    # `stat=600 root:root` -> 'root:root'
    $remotePerm = if ($statLine) { (($statLine -replace '^stat=', '') -split '\s+')[1] } else { '' }

    if ($null -eq $remoteToc) {
        Add-Check 'ledger-offhost' 'fail' "VPS has $name but no readable manifest beside it"
    } elseif ($remoteToc -lt 100) {
        Add-Check 'ledger-offhost' 'fail' "VPS copy $name records only $remoteToc TOC entries"
    } elseif ($remotePerm -and $remotePerm -ne 'root:root') {
        Add-Check 'ledger-offhost' 'fail' "VPS copy $name is owned by $remotePerm; expected root:root"
    } else {
        # Freshness comes from the local side: the push stamps the log, and the
        # remote mtime of a copied file is the transfer time, not the backup
        # time. The newest local sealed artefact is the honest age signal, and
        # it was measured above.
        $ageH = if ($newestLedger) { [int](((Get-Date) - $newestLedger.LastWriteTime).TotalHours) } else { 999 }
        $desc = "$name ($remoteToc TOC entries, $remotePerm)"
        if ($ageH -ge $offhostFail.TotalHours) {
            Add-Check 'ledger-offhost' 'fail' "VPS copy is $ageH h behind the newest local backup ($desc)"
        } elseif ($ageH -ge $offhostWarn.TotalHours) {
            Add-Check 'ledger-offhost' 'warn' "VPS copy is $ageH h behind the newest local backup ($desc)"
        } else {
            Add-Check 'ledger-offhost' 'ok' "$desc, within $ageH h of the local backup"
        }
    }
}

# --- report ----------------------------------------------------------------
$worst = 'ok'
foreach ($c in $checks) {
    if ($c.state -eq 'fail') { $worst = 'fail'; break }
    if ($c.state -eq 'warn' -and $worst -ne 'fail') { $worst = 'warn' }
}

$report = [ordered]@{
    checked_at = (Get-Date).ToString('o')
    overall    = $worst
    checks     = $checks
}
$report | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $statusFile -Encoding utf8

if ($Json) {
    $report | ConvertTo-Json -Depth 6
} else {
    Write-Host "memgw health: $worst  ($($report.checked_at))"
    foreach ($c in $checks) {
        $tag = switch ($c.state) { 'ok' { '  OK  ' } 'warn' { ' WARN ' } 'fail' { ' FAIL ' } }
        Write-Host "$tag $($c.check.PadRight(20)) $($c.detail)"
    }
    Write-Host "status file: $statusFile"
}

if ($worst -eq 'fail') { exit 2 }
if ($worst -eq 'warn') { exit 1 }
exit 0
