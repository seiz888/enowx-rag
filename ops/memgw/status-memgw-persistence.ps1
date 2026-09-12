<#
.SYNOPSIS
    Report the memgw pilot's persistence and health.

.DESCRIPTION
    Answers four questions an operator actually asks, and nothing else:

      1. Are the Scheduled Tasks registered, and when did they last run?
      2. Are the gateway and collectors running right now, as processes?
      3. Is the gateway answering HTTP?
      4. What do the collector queues say -- pending, dead, quarantined?

    A process being alive says nothing about whether it works, so the HTTP
    probe and the queue counts are reported separately from the process list.
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

$gatewayReady = Test-MemgwGatewayReady -TimeoutSeconds 3 -IntervalMs 200

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
        gateway_ready = $gatewayReady
        gateway_addr  = $script:MemgwLocalGatewayAddr
        gateway_dsn   = Get-MemgwRedactedDsn
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
Write-Host "  address : $script:MemgwLocalGatewayAddr"
Write-Host "  dsn     : $(Get-MemgwRedactedDsn)  (redacted; no credential shown)"
Write-Host "  ready   : $gatewayReady"
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
