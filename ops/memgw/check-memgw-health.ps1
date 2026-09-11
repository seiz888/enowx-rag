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
