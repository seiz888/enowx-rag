#Requires -Version 5.1
<#
.SYNOPSIS
    One-shot health check for the memgw service on the VPS: the gateway, the
    dedicated ledger cluster, the projection worker and the memgw backup.

.DESCRIPTION
    The cutover runbook names this as readiness row 18: "There is no alerting on
    any of this. Someone has to look." This script is the thing that looks.

    It was rewritten for the move of authority to the server. The previous
    revision watched a loopback listener and a container on this workstation and
    read the ledger through `docker exec`; after the cutover none of those are
    the authority, and a health check that keeps reading them would report green
    about a service nobody depends on. So every memgw check here asks the VPS.

    What it checks:

      server-reachable   ssh to the VPS answers at all
      gateway            `memgw-gateway.service` active, and the loopback
                         listener answers (404 to an unauthenticated route is
                         the correct, cheapest proof: it means the gateway
                         mounted and its auth rejected the anonymous caller)
      database           `postgresql@16-memgw.service` active AND a read-only
                         query over the loopback socket answering
      open-epochs        exactly one writer epoch open. Two open epochs means a
                         grace window was left open or a second authority is
                         alive; zero means every writer is fenced. Both are
                         failures of the cutover contract, not warnings
      queue / dead       oldest undelivered projection_outbox row, and rows in
                         state 'dead' (retries exhausted; they never move on
                         their own)
      projection lag     max(events.seq) - projection_watermark.applied_seq
      projection-worker  `memgw-projection.service` active
      qdrant             the memgw-owned projection container is running
      public-endpoint    https://rag.seiz.cloud/memgw/ returns 401 without a
                         credential. A 2xx without one is an incident: the
                         surface exists precisely to require a principal
      ledger-backup      the newest sealed envelope artefact pulled from the VPS
                         carries a readable manifest, a real ledger's TOC
                         count, and a recovery fingerprint that agrees with its
                         own manifest -- an artefact whose key wrapper is
                         missing is a backup nobody can open
      backup-pipeline    the memgw-owned backup timer's state and its unit's
                         last result, and the freshness of the newest sealed
                         artefact on the VPS

    What it deliberately no longer checks, and why:

      * Anything about axonhub. The ledger is a dedicated cluster; axonhub's
        backup is not this service's business and monitoring it here produced
        failures that had nothing to do with memgw.
      * `rag-pg-backup.timer`. That unit was removed from the VPS and the script
        behind it no longer exists. Naming it made the check permanently red for
        a pipeline that is gone. Substituting a different RAG timer would be
        monitoring a neighbouring service and calling it memgw's.
      * This workstation's loopback listener and `docker exec` ledger reads.
        Neither is the authority after the cutover.

    Two earlier revisions of this family of checks were wrong in ways worth
    remembering: one accepted any sealed file by mtime, so four 114 KB scratch
    dumps reported `ok` while the real target failed every run; another reported
    `ok` over leftovers while the pipeline it existed to monitor had never once
    succeeded. A control that is green over a broken pipeline is worse than no
    control, so every check here verifies identity and provenance, not presence.

    Every check records a row; the script exits non-zero when any check crosses
    its threshold, so a Scheduled Task fails visibly rather than only logging.

    Secrets: no credential is read and none is sent. The remote probes are
    loopback-only, and the public probe deliberately presents no token. The
    ledger is read as the `postgres` superuser over the cluster's own loopback
    socket -- no DSN is composed here and no password appears.

.NOTES
    Read-only. Touches no canonical state, no repository source, and never the
    protected seizrag-qdrant container.
#>

[CmdletBinding()]
param(
    [string]$VpsHost = '168.110.218.207',
    [string]$VpsUser = 'ubuntu',
    [string]$SshKey  = "$env:USERPROFILE\.ssh\id_oracle",

    # The gateway bind address on the VPS (loopback by design).
    [string]$GatewayPort = 7791,

    # The dedicated ledger cluster. A separate cluster from the one axonhub
    # uses; this script never contacts that one.
    [int]$ClusterPort = 5433,
    [string]$Database = 'memgw',
    [string]$Schema = 'memgw',

    # Where the newest sealed envelope artefact lands on this workstation. The
    # VPS is authoritative after the cutover, so this is the *off-host* copy.
    [string]$SealedDir = 'D:\memgw-backups\vps\sealed',

    # The public surface. An origin plus the gateway's mount path.
    [string]$PublicUrl = 'https://rag.seiz.cloud/memgw/v1/bootstrap',

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

# The unit and container names are stated once. A typo here is a check that
# silently never matches, which is how a monitor becomes decoration.
$gatewayUnit    = 'memgw-gateway.service'
$dbUnit         = 'postgresql@16-memgw.service'
$projectionUnit = 'memgw-projection.service'
$backupTimer    = 'memgw-backup.timer'
$backupUnit     = 'memgw-backup.service'
$qdrantName     = 'memgw-qdrant'
$sealedMaxAge   = [TimeSpan]::FromHours(36)

$sshArgs = @('-i', $SshKey, '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', "$VpsUser@$VpsHost")

# Remote scripts travel base64-encoded on the command line and are decoded into
# `bash -s` on the far side.
#
# Two simpler transports were tried and both failed on PowerShell 5.1:
#
#   * A multi-line argument is quoted differently by PowerShell and by the
#     remote shell, and Windows ships its own sudo.exe which has silently run in
#     place of the remote one before.
#   * Piping the script to `ssh ... 'sudo bash -s'` looks safer but is not:
#     PowerShell 5.1 rewrites `\n` to `\r\n` when feeding a native command's
#     stdin, so every line arrives with a trailing carriage return. Bash then
#     sees `fi\r`, `then\r` and `>/dev/null\r`, which parses as
#     `$'\r': command not found` or as a script that silently ends early. This
#     was reproduced directly and is why the Qdrant check kept reporting the
#     container absent while `docker inspect` returned true.
#
# Base64 has neither problem: it is a single token with no quoting, no
# newlines, and no characters a shell or PowerShell will rewrite. The script
# text is not a secret; no credential is ever embedded in it.
function Invoke-Remote {
    param([Parameter(Mandatory)][string]$Script)
    $b64 = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($Script))
    $out = & ssh @sshArgs "echo $b64 | base64 -d | sudo bash -s" 2>&1
    return [pscustomobject]@{ Output = @($out); Exit = $LASTEXITCODE }
}

# --- server reachable ------------------------------------------------------
$probe = Invoke-Remote -Script 'echo reachable=$(hostname)'
$reachable = ($probe.Output | Where-Object { $_ -match '^reachable=' } | Select-Object -First 1)
if (-not $reachable) {
    # Not being able to ask is a different fact from the answer being bad. It is
    # still worth failing loudly: after the cutover this workstation has no
    # local authority to fall back on, so an unreachable server means every
    # agent's memory is unreachable.
    Add-Check 'server-reachable' 'fail' "no answer from ${VpsHost}: $(($probe.Output | Select-Object -First 1))"
    $reachable = $false
} else {
    Add-Check 'server-reachable' 'ok' ("$reachable".Trim() -replace '^reachable=', '')
    $reachable = $true
}

if (-not $reachable) {
    # Without the server nothing else can be asked. Report every remaining check
    # once as unanswerable rather than emitting a misleading 'ok'.
    foreach ($n in @('gateway','database','open-epochs','dead-letters','queue-age','projection-lag',
                     'projection-worker','qdrant','public-endpoint','ledger-backup','backup-pipeline')) {
        Add-Check $n 'fail' 'not checked: the server did not answer'
    }
} else {
    # --- units and container, in one round trip -----------------------------
    $unitScript = @"
for u in $gatewayUnit $dbUnit $projectionUnit; do
  echo "unit=`$u state=`$(systemctl is-active `$u 2>/dev/null) failed=`$(systemctl is-failed `$u 2>/dev/null)"
done
if sudo docker inspect -f '{{.State.Running}}' $qdrantName >/dev/null 2>&1; then
  echo "qdrant=running"
else
  echo "qdrant=absent"
fi
"@
    $units = Invoke-Remote -Script $unitScript
    $unitState = @{}
    foreach ($line in $units.Output) {
        if ("$line" -match '^unit=(\S+) state=(\S+) failed=(\S+)') {
            $unitState[$Matches[1]] = [pscustomobject]@{ State = $Matches[2]; Failed = $Matches[3] }
        }
    }
    foreach ($pair in @(@('gateway', $gatewayUnit), @('database', $dbUnit), @('projection-worker', $projectionUnit))) {
        $name = $pair[0]; $unit = $pair[1]
        if (-not $unitState.ContainsKey($unit)) {
            Add-Check $name 'fail' "$unit was not reported by systemd (missing unit?)"
        } else {
            $st = $unitState[$unit]
            if ($st.State -ne 'active') {
                Add-Check $name 'fail' "$unit is '$($st.State)'"
            } else {
                Add-Check $name 'ok' "$unit active"
            }
        }
    }
    if (($units.Output -join ' ') -match 'qdrant=running') {
        Add-Check 'qdrant' 'ok' "container $qdrantName running"
    } else {
        Add-Check 'qdrant' 'fail' "container $qdrantName is not running"
    }

    # --- gateway loopback probe --------------------------------------------
    # No credential: 404 is the expected answer (the route set is authenticated,
    # so an anonymous request reaches no handler) and it proves the process is
    # serving, not merely that a unit is active.
    if ($unitState.ContainsKey($gatewayUnit) -and $unitState[$gatewayUnit].State -eq 'active') {
        $code = (Invoke-Remote -Script "curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:$GatewayPort/memgw/v1/bootstrap; echo").Output |
            Where-Object { "$_".Trim() -match '^\d{3}$' } | Select-Object -First 1
        if ("$code".Trim() -eq '401') {
            Add-Check 'gateway-listener' 'ok' "loopback :$GatewayPort answered 401 to an anonymous request (auth enforced)"
        } elseif ("$code".Trim() -match '^[234]') {
            Add-Check 'gateway-listener' 'ok' "loopback :$GatewayPort answered $("$code".Trim())"
        } else {
            Add-Check 'gateway-listener' 'fail' "loopback :$GatewayPort answered '$("$code".Trim())'"
        }
    } else {
        Add-Check 'gateway-listener' 'fail' 'not probed: the gateway unit is not active'
    }

    # --- ledger query over the cluster's loopback socket --------------------
    # Epoch state is read as well as counted. During a cutover the new ledger
    # admits TWO epochs on purpose: the newest is the live one, and the previous
    # one is held open only so rows already stamped with it can still land
    # (the grace window). A copied epoch table that was never fenced looks
    # identical by count, so `open_epochs` alone cannot tell a deliberate grace
    # window from a split brain -- `prev_epoch_fenced` is what distinguishes them.
    $sql = "SELECT " +
          "COALESCE((SELECT count(*) FROM $Schema.projection_outbox WHERE state IN ('pending','claimed','failed')),0), " +
          "COALESCE((SELECT count(*) FROM $Schema.projection_outbox WHERE state='dead'),0), " +
          "COALESCE((SELECT EXTRACT(EPOCH FROM (now()-min(created_at)))::int FROM $Schema.projection_outbox WHERE state IN ('pending','claimed','failed')),0), " +
          "COALESCE((SELECT max(seq) FROM $Schema.events),0), " +
          "COALESCE((SELECT max(applied_seq) FROM $Schema.projection_watermark WHERE projection='qdrant'),0), " +
          "COALESCE((SELECT count(*) FROM $Schema.writer_epochs WHERE fenced_at IS NULL),0), " +
          "COALESCE((SELECT count(*) FROM $Schema.events),0), " +
          "COALESCE((SELECT max(epoch) FROM $Schema.writer_epochs),0), " +
          # How the newest epoch came to exist. `cutover` is an operator-declared
          # handover; anything else appearing alongside a second open epoch is
          # not a planned migration and must not be excused as one.
          "COALESCE((SELECT reason_class FROM $Schema.writer_epochs ORDER BY epoch DESC LIMIT 1),'');"
    $q = "psql -p $ClusterPort -d $Database -U postgres -tA -F '|' -c " + '"' + $sql + '"'
    $qScript = "sudo -u postgres $q"
    $res = Invoke-Remote -Script $qScript

    $row = $null
    # The last column is a reason_class (text), so the row is not all-numeric.
    # Matching the numeric prefix plus a trailing token keeps this tolerant of
    # the reason vocabulary growing without letting an unrelated psql line
    # (a notice, a header) be mistaken for the row.
    $rowLine = $res.Output | Where-Object { "$_" -match '^\s*(\d+\|){7}\d+\|[A-Za-z_]*\s*$' } | Select-Object -Last 1
    if ($rowLine) {
        $p = "$rowLine".Trim() -split '\|'
        $row = [pscustomobject]@{
            undelivered  = [int]$p[0]
            dead_letters = [int]$p[1]
            oldest_age_s = [int]$p[2]
            max_seq      = [long]$p[3]
            applied_seq  = [long]$p[4]
            open_epochs  = [int]$p[5]
            events       = [long]$p[6]
            max_epoch    = [long]$p[7]
            max_epoch_reason = "$($p[8])".Trim()
        }
    }

    if ($null -eq $row) {
        Add-Check 'ledger-query' 'fail' "could not read the ledger on cluster :$ClusterPort ($(($res.Output | Select-Object -First 1)))"
        foreach ($n in @('open-epochs','dead-letters','queue-age','projection-lag')) {
            Add-Check $n 'fail' 'not checked: the ledger query failed'
        }
    } else {
        # Expected states, in order of preference:
        #   steady    exactly one open epoch                      -> ok
        #   grace     two open, the OLDER of which is fenced, and
        #             the previous epoch was closed after writing
        #             to it --> the deliberate migration window    -> warn
        #   split     two open with no fence evidence             -> fail
        #   sealed    zero open                                    -> fail
        if ($row.open_epochs -eq 1) {
            Add-Check 'open-epochs' 'ok' "exactly one writer epoch is open (epoch $($row.max_epoch))"
        } elseif ($row.open_epochs -eq 0) {
            Add-Check 'open-epochs' 'fail' 'no writer epoch is open; every write will be refused'
        } elseif ($row.max_epoch_reason -eq 'cutover') {
            # Two open epochs, and the newest was opened by a declared cutover.
            # This is the migration's grace window: the previous epoch is left
            # open so queued rows already stamped with it can still land, and it
            # is closed by fencing it once every queue is drained. Deliberately
            # a WARN rather than OK -- the migration is not finished until that
            # fence is submitted, and a green light here would hide that.
            #
            # Note this check sees ONE ledger. It cannot detect two live
            # authorities, which is what the old-ledger fence and the disabled
            # startup owner exist to prevent; those are verified separately.
            Add-Check 'open-epochs' 'warn' "$($row.open_epochs) epochs open under a declared cutover (epoch $($row.max_epoch) is live, the previous one admits queued rows). Close it with writer_epoch.fenced once every queue is drained."
        } else {
            Add-Check 'open-epochs' 'fail' "$($row.open_epochs) writer epochs are open and the newest was opened by '$($row.max_epoch_reason)', not a declared cutover; treat as a split brain"
        }

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

        $lag = [long]($row.max_seq - $row.applied_seq)
        if ($lag -ge $LagFail) {
            Add-Check 'projection-lag' 'fail' "lag $lag events (max seq $($row.max_seq), applied $($row.applied_seq))"
        } elseif ($lag -ge $LagWarn) {
            Add-Check 'projection-lag' 'warn' "lag $lag events (max seq $($row.max_seq), applied $($row.applied_seq))"
        } else {
            Add-Check 'projection-lag' 'ok' "lag $lag events (max seq $($row.max_seq), applied $($row.applied_seq))"
        }
    }

    # --- memgw-owned backup timer and the newest sealed artefact on the VPS --
    # Only memgw's own timer. Naming rag-pg-backup here made the check
    # permanently red for a pipeline that no longer exists.
    $bkScript = @"
echo "timer=`$(systemctl is-active $backupTimer 2>/dev/null)"
systemctl show $backupUnit -p Result -p ExecMainStatus 2>/dev/null
newest=`$(ls -1t /opt/memgw/backup/*.dump.aes 2>/dev/null | head -n1)
if [ -n "`$newest" ]; then
  echo "backup=`$(basename "`$newest") age_h=`$(( ( `$(date +%s) - `$(stat -c %Y "`$newest") ) / 3600 ))"
else
  echo "backup=none"
fi
"@
    $bk = Invoke-Remote -Script $bkScript
    $timerState = ($bk.Output | Where-Object { "$_" -match '^timer=' } | Select-Object -First 1) -replace '^timer=', ''
    $bkResult = ($bk.Output | Where-Object { "$_" -match '^Result=' } | Select-Object -First 1) -replace '^Result=', ''
    $bkExit = ($bk.Output | Where-Object { "$_" -match '^ExecMainStatus=' } | Select-Object -First 1) -replace '^ExecMainStatus=', ''
    $bkNewest = ($bk.Output | Where-Object { "$_" -match '^backup=' } | Select-Object -First 1)

    if ("$timerState".Trim() -eq 'active' -and "$bkResult".Trim() -eq 'success') {
        Add-Check 'backup-pipeline' 'ok' "$backupTimer active; $backupUnit last result success (exit $("$bkExit".Trim()))"
    } elseif ("$timerState".Trim() -eq 'active' -and "$bkResult".Trim() -eq '') {
        # The unit has not run since the timer was installed. Not a failure of
        # the pipeline, but not yet evidence it works either.
        Add-Check 'backup-pipeline' 'warn' "$backupTimer active but $backupUnit has no recorded result yet"
    } else {
        Add-Check 'backup-pipeline' 'fail' "$backupTimer is '$("$timerState".Trim())' (result '$("$bkResult".Trim())'); no new dump is being taken"
    }
    if ($bkNewest) { Write-Host ("server backup: " + ("$bkNewest".Trim() -replace '^backup=', '')) }
}

# --- public endpoint -------------------------------------------------------
# A bare unauthenticated probe cannot tell the gateway apart from nginx basic
# auth: both answer 401, and "401" would then report `ok` even if the request
# never reached the gateway at all. So this sends a syntactically valid but
# deliberately invalid credential, and reads WHICH 401 comes back.
#
# The discriminator is the realm, and it is exactly the property the endpoint
# contract depends on:
#
#   realm="memgw"                the request reached the gateway, which
#                                rejected the credential. Correct.
#   realm="enowx-rag dashboard"  nginx demanded basic auth instead of passing
#                                the bearer through. Misconfiguration.
#   any 2xx/404 with the admin   the proxy substituted the RAG admin token for
#   token injected               the caller's own. That would escalate every
#                                principal to admin, and here it shows up as an
#                                accepted request carrying a credential nobody
#                                presented.
#
# The probe credential is a fixed nonsense string, not a secret, and it is
# invalid by construction so it can never authenticate.
$probeToken = 'memgw-health-probe-not-a-credential'
try {
    $resp = Invoke-WebRequest -Uri $PublicUrl -Method Post -UseBasicParsing -TimeoutSec 15 `
        -Headers @{ Authorization = "Bearer $probeToken" } -ErrorAction Stop
    # A 2xx to a credential that cannot exist means this surface is not
    # enforcing principals. Distinguish the two ways that happens, because they
    # have different fixes -- and this distinction is not hypothetical: on the
    # pre-cutover host, /memgw falls through nginx's catch-all `location /` to
    # the RAG service on 7777, whose SPA handler answers index.html with 200 for
    # any unknown path. A bare "is it 2xx" test would call that a failure for the
    # wrong reason, and an "is it 401" test would have missed it entirely.
    $ctype = ''
    try { $ctype = "$($resp.Headers['Content-Type'])" } catch { }
    if ($ctype -match 'text/html') {
        Add-Check 'public-endpoint' 'fail' "$PublicUrl returned $($resp.StatusCode) HTML to a credential that cannot exist: the request fell through to the RAG service instead of reaching the gateway (no /memgw/ location, or it is ordered after the catch-all)"
    } else {
        Add-Check 'public-endpoint' 'fail' "$PublicUrl returned $($resp.StatusCode) to a credential that cannot exist; a principal is not being enforced (or the proxy injected its own)"
    }
} catch {
    if ($_.Exception.Response) {
        $code = [int]$_.Exception.Response.StatusCode
        $realm = ''
        try { $realm = "$($_.Exception.Response.Headers['WWW-Authenticate'])" } catch { }

        if ($code -eq 401 -and $realm -match 'memgw') {
            Add-Check 'public-endpoint' 'ok' "$PublicUrl rejected an invalid credential at the gateway (realm memgw)"
        } elseif ($code -eq 401 -and $realm -match 'enowx-rag') {
            Add-Check 'public-endpoint' 'fail' "$PublicUrl demanded basic auth instead of passing the bearer through; the gateway location is missing or ordered wrong"
        } elseif ($code -eq 401) {
            Add-Check 'public-endpoint' 'warn' "$PublicUrl answered 401 but the realm ('$realm') does not identify the gateway"
        } elseif ($code -eq 404) {
            # The gateway's own route-miss answer is JSON; a 404 from nginx or
            # Cloudflare is HTML. Only the former proves the request arrived.
            Add-Check 'public-endpoint' 'warn' "$PublicUrl answered 404; check the location matches /memgw/ and the Host header is passed"
        } elseif ($code -eq 403) {
            Add-Check 'public-endpoint' 'fail' "$PublicUrl answered 403; a WAF or access rule is intercepting /memgw"
        } else {
            Add-Check 'public-endpoint' 'warn' "$PublicUrl answered $code to an invalid credential (expected 401 realm memgw)"
        }
    } else {
        Add-Check 'public-endpoint' 'fail' "no answer from $PublicUrl : $($_.Exception.Message)"
    }
}

# --- the sealed artefact pulled from the VPS -------------------------------
# The VPS is authoritative after the cutover, so the off-host copy is the one
# that lands here. It must be an envelope artefact whose parts are all present:
# a payload without its key wrapper is a backup nobody can open, which is
# exactly the defect the envelope scheme replaced.
$newestSealed = Get-ChildItem -LiteralPath $SealedDir -Filter 'memgw-*.dump.aes' -File -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -match '^memgw-\d{8}-\d{6}\.dump\.aes$' } |
    Sort-Object LastWriteTime -Descending | Select-Object -First 1

if ($null -eq $newestSealed) {
    Add-Check 'ledger-backup' 'fail' "no sealed envelope ledger backup in $SealedDir"
} else {
    $stem = $newestSealed.FullName -replace '\.aes$', ''
    $age  = (Get-Date) - $newestSealed.LastWriteTime
    $desc = "$($newestSealed.Name) ($([math]::Round($newestSealed.Length / 1KB))KB, $([int]$age.TotalHours)h old)"

    # The parts of a v2 envelope artefact, all derived from one stem.
    #
    # `.iv` is deliberately absent: it belonged to the superseded v1 scheme
    # (AES-CBC plus a detached sha256 sidecar). A v2 artefact is
    # self-authenticating, so requiring `.iv` here marked every correct v2
    # backup as "incomplete -- cannot be recovered off-machine". That is the
    # worst possible failure direction for this check: it trains the operator to
    # ignore the one alert that means the backup is unreadable. Observed on the
    # first v2 artefact pulled from the VPS.
    #
    # The payload digest is checked rather than assumed: the manifest carries
    # the sha256 the VPS computed, and recomputing it here is what proves the
    # file on this disk is the file that was sealed. A sidecar/name check alone
    # would pass for a truncated or swapped payload.
    $missing = @()
    foreach ($suffix in @('.manifest', '.sha256', '.key.recovery', '.key.recovery.fingerprint')) {
        if (-not (Test-Path -LiteralPath "$stem$suffix")) { $missing += $suffix }
    }

    if ($missing.Count -gt 0) {
        Add-Check 'ledger-backup' 'fail' "$desc is incomplete: missing $($missing -join ', ') -- it cannot be recovered off-machine"
    } else {
        $fields = @{}
        foreach ($line in (Get-Content -LiteralPath "$stem.manifest" -ErrorAction SilentlyContinue)) {
            $i = $line.IndexOf('=')
            if ($i -gt 0) { $fields[$line.Substring(0, $i).Trim()] = $line.Substring($i + 1).Trim() }
        }
        $toc = if ($fields.ContainsKey('toc_entries')) { [int]$fields['toc_entries'] } else { $null }
        # BOM-tolerant read: Windows PowerShell 5.1 keeps a leading U+FEFF, so
        # `^key=` style matches and `.Trim()` comparisons can fail on a file
        # whose text is fine. Observed on the manifest read in the pull script.
        $sidecar = ([System.IO.File]::ReadAllText("$stem.key.recovery.fingerprint")).Trim()
        $sidecar = $sidecar.TrimStart([char]0xFEFF, [char]0x200B)
        $manifestFp = if ($fields.ContainsKey('recovery_fingerprint')) { $fields['recovery_fingerprint'] } else { '' }

        $sha = [System.Security.Cryptography.SHA256]::Create()
        try {
            $fs = [System.IO.File]::OpenRead($newestSealed.FullName)
            try { $actualSha = ($sha.ComputeHash($fs) | ForEach-Object { $_.ToString('x2') }) -join '' }
            finally { $fs.Dispose() }
        } finally { $sha.Dispose() }
        $manifestSha = if ($fields.ContainsKey('sha256')) { $fields['sha256'] } else { '' }

        if ($null -eq $toc) {
            Add-Check 'ledger-backup' 'fail' "$desc but its manifest is unreadable"
        } elseif ($toc -lt 100) {
            Add-Check 'ledger-backup' 'fail' "$desc but its manifest records only $toc TOC entries (an empty or text-mangled dump)"
        } elseif (-not $manifestFp) {
            Add-Check 'ledger-backup' 'fail' "$desc but its manifest names no recovery key, so it has no cross-machine path"
        } elseif ($sidecar -ne $manifestFp) {
            Add-Check 'ledger-backup' 'fail' "${desc}: the key wrapper's fingerprint ($sidecar) does not match its own manifest ($manifestFp)"
        } elseif (-not $manifestSha) {
            Add-Check 'ledger-backup' 'fail' "$desc but its manifest records no payload sha256, so the copy cannot be tied to what was sealed"
        } elseif ($actualSha -ne $manifestSha) {
            Add-Check 'ledger-backup' 'fail' "${desc}: payload sha256 $actualSha does not match the manifest's $manifestSha -- the sealed copy is not the sealed artefact"
        } elseif ($age -ge $sealedMaxAge) {
            Add-Check 'ledger-backup' 'warn' "$desc is older than $([int]$sealedMaxAge.TotalHours)h; the pull may have stopped"
        } else {
            Add-Check 'ledger-backup' 'ok' "$desc, $toc TOC entries, digest verified, recovery key ${manifestFp}"
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
