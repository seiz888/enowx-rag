<#
.SYNOPSIS
    Report the memgw pilot's persistence and health.

.DESCRIPTION
    Answers four questions an operator actually asks, and nothing else:

      1. Are the Scheduled Tasks registered, and when did they last run?
      2. Are the gateway and collectors running right now, as processes?
      3. Is the gateway the collectors forward to actually answering?
      4. What do the collector queues say -- pending, dead, quarantined?

    A process being alive says nothing about whether it works, so the reachability
    probe and the queue counts are reported separately from the process list.

    The probe follows the SUPERVISOR'S MODE, because "the gateway" means a
    different endpoint in each. In collectors-only mode (the ordinary install)
    the authority is remote, so probing the retired local address would report
    "not ready" every single time an operator looked -- a permanent false alarm
    that trains readers to ignore the one field that matters. In
    gateway+collectors mode the local address is the right thing to probe.

    Prints no secret: the DSN is shown redacted and collector credentials are
    named by path only.

.PARAMETER Json
    Emit machine-readable output instead of the human table.
#>

[CmdletBinding()]
param([switch]$Json)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'Memgw.Common.ps1')

$taskPath = '\memgw\'

# --- Tasks -----------------------------------------------------------------

$tasks = @()
foreach ($t in (Get-ScheduledTask -TaskPath $taskPath -ErrorAction SilentlyContinue)) {
    $info = Get-ScheduledTaskInfo -TaskPath $t.TaskPath -TaskName $t.TaskName -ErrorAction SilentlyContinue
    $tasks += [PSCustomObject]@{
        Name        = $t.TaskName
        State       = [string]$t.State
        LastRun     = if ($info) { $info.LastRunTime } else { $null }
        LastResult  = if ($info) { $info.LastTaskResult } else { $null }
        NextRun     = if ($info) { $info.NextRunTime } else { $null }
    }
}

# --- Processes -------------------------------------------------------------

$procs = Get-MemgwRunningProcesses

# --- Gateway readiness -----------------------------------------------------

# Which endpoint is "the gateway" depends on how the supervisor is registered,
# so read the mode from the task rather than assuming.
$supervisorTask = Get-ScheduledTask -TaskPath $taskPath -TaskName 'supervisor' -ErrorAction SilentlyContinue
$collectorsOnly = $false
if ($supervisorTask) {
    $collectorsOnly = [bool]($supervisorTask.Actions[0].Arguments -match '-CollectorsOnly')
}

$gatewayTarget = if ($collectorsOnly) { $script:MemgwGatewayUrl } else { $script:MemgwLocalGatewayUrl }
$gatewayReady  = Test-MemgwGatewayReadyTo -Url $gatewayTarget -TimeoutSeconds 3 -IntervalMs 200

# --- Collector queues ------------------------------------------------------

$queues = @()
foreach ($c in $script:MemgwCollectors) {
    $stats = Get-MemgwCollectorStats -Collector $c
    $queues += [PSCustomObject]@{
        Name        = $c.Name
        Spool       = $c.Dir
        Pipe        = $c.Pipe
        PipeExists  = (Test-Path -LiteralPath $c.Pipe)
        Pending     = if ($stats) { $stats.pending } else { $null }
        Sending     = if ($stats) { $stats.sending } else { $null }
        Sent        = if ($stats) { $stats.sent } else { $null }
        Quarantined = if ($stats) { $stats.quarantined } else { $null }
        Dead        = if ($stats) { $stats.dead } else { $null }
    }
}

# --- Report ----------------------------------------------------------------

if ($Json) {
    [PSCustomObject]@{
        tasks         = $tasks
        processes     = $procs
        supervisor_mode = if ($collectorsOnly) { 'collectors-only' } else { 'gateway+collectors' }
        gateway_ready = $gatewayReady
        gateway_target = $gatewayTarget
        # Only meaningful in the local mode: in collectors-only there is no local
        # DSN in play, and printing one would imply a local ledger is in use.
        gateway_dsn   = if ($collectorsOnly) { $null } else { Get-MemgwRedactedDsn }
        queues        = $queues
        checked_at    = (Get-Date).ToString('o')
    } | ConvertTo-Json -Depth 6
    exit 0
}

Write-Host '=== memgw persistence ==='
if ($tasks.Count -eq 0) {
    Write-Host '  (no tasks registered under \memgw\)'
} else {
    $tasks | Format-Table Name, State, LastRun, LastResult -AutoSize | Out-String | Write-Host
}

Write-Host '=== processes ==='
if (-not $procs) {
    Write-Host '  (none running)'
} else {
    $procs | Format-Table Kind, Name, ProcId, Started -AutoSize | Out-String | Write-Host
}

Write-Host '=== gateway ==='
Write-Host "  supervisor mode : $(if ($collectorsOnly) { 'collectors-only' } else { 'gateway+collectors' })"
Write-Host "  probing         : $gatewayTarget"
if ($collectorsOnly) {
    # The local address is deliberately NOT probed here: in this mode the local
    # gateway is retired, so reporting it as "not ready" would be a permanent
    # false alarm on a correctly-installed client.
    Write-Host "  local gateway   : not probed (retired; the authority is the endpoint above)"
} else {
    Write-Host "  dsn             : $(Get-MemgwRedactedDsn)  (redacted; no credential shown)"
}
Write-Host "  ready           : $gatewayReady"
Write-Host ''

Write-Host '=== collector queues ==='
if ($queues.Count -eq 0) {
    Write-Host '  (none configured)'
} else {
    $queues | Format-Table Name, PipeExists, Pending, Sending, Sent, Quarantined, Dead -AutoSize | Out-String | Write-Host
}

$bad = $queues | Where-Object { $_.Quarantined -gt 0 -or $_.Dead -gt 0 }
if ($bad) {
    Write-Host 'WARNING: a collector holds quarantined or dead rows.'
    Write-Host '  Inspect with: enowx-rag memgw collector held --dir <spool>'
    Write-Host '  Runbook: docs/runbooks/memgw-collector.md'
}
