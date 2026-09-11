<#
.SYNOPSIS
    Remove the memgw pilot's persistence. Reverses install-memgw-persistence.ps1.

.DESCRIPTION
    Removes exactly the Scheduled Tasks recorded in the install manifest, and
    nothing else. If no manifest is present, it removes nothing and says so.

    Deliberately conservative:

      - Refuses to remove a task whose action hash no longer matches the
        manifest. A task edited by hand is not the task this script installed,
        and deleting it silently would destroy somebody else's change.
      - Never touches spools, logs, secrets, the database, or the binary. Those
        are data; this script only stops the machine from starting them.
      - Never reboots, never kills a process unless -StopProcesses is given
        explicitly, and then only processes matching the pilot binary path.

.PARAMETER StopProcesses
    Also stop the currently running gateway and collector processes. Without
    this switch the tasks are removed but running processes are left alone.

.PARAMETER WhatIf
    Report what would be removed without removing it.
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [switch]$StopProcesses
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'Memgw.Common.ps1')

$manifestPath = Join-Path $script:MemgwRoot 'run\installed-tasks.json'
$stopFile     = Join-Path $script:MemgwRoot 'run\supervisor.stop'

Write-Host 'memgw persistence uninstall'
Write-Host ''

if (-not (Test-Path -LiteralPath $manifestPath)) {
    Write-Host "No manifest at $manifestPath -- nothing was installed by this toolkit."
    Write-Host 'Removing nothing. If tasks exist under \memgw\, remove them deliberately.'
    exit 0
}

$manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json

Write-Host "  installed at : $($manifest.installed_at)"
Write-Host "  installed by : $($manifest.installed_by)"
Write-Host "  task         : $($manifest.task_path)$($manifest.task_name)"
Write-Host ''

# Tell a running supervisor to exit at its next poll before removing the task,
# so it does not restart what we are about to stop.
if (Test-Path -LiteralPath $stopFile) {
    Write-Host '  stop file already present'
} elseif ($PSCmdlet.ShouldProcess($stopFile, 'create supervisor stop file')) {
    Set-Content -LiteralPath $stopFile -Value 'stop' -Encoding utf8
    Write-Host "  stop file written: $stopFile"
}

Start-Sleep -Seconds 2

# --- Remove the recorded task ---------------------------------------------

$taskPath = $manifest.task_path
$taskName = $manifest.task_name
$task = Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName -ErrorAction SilentlyContinue

if (-not $task) {
    Write-Host "  task $taskPath$taskName is already absent"
} else {
    $actualHash = Get-MemgwTaskHash -Task $task
    if ($actualHash -ne $manifest.action_hash) {
        Write-Host 'REFUSING: the task action does not match the install manifest.'
        Write-Host "  recorded : $($manifest.action_hash)"
        Write-Host "  actual   : $actualHash"
        Write-Host '  Somebody edited this task after install. Inspect it and remove it by hand.'
        exit 2
    }
    if ($PSCmdlet.ShouldProcess("$taskPath$taskName", 'unregister scheduled task')) {
        Unregister-ScheduledTask -TaskPath $taskPath -TaskName $taskName -Confirm:$false
        Write-MemgwLog -Name 'ops' -Message "uninstall: removed task $taskPath$taskName"
        Write-Host "  removed task: $taskPath$taskName"
    }
}

# Remove the task folder if it is now empty, so a reinstall starts clean.
$folder = $taskPath.TrimEnd('\')
if ($folder) {
    $remaining = Get-ScheduledTask -TaskPath $taskPath -ErrorAction SilentlyContinue
    if (-not $remaining) {
        try {
            $svc = New-Object -ComObject Schedule.Service
            $svc.Connect()
            $root = $svc.GetFolder('\')
            $root.DeleteFolder($folder, 0)
            Write-Host "  removed empty task folder: $taskPath"
        } catch {
            Write-Host "  task folder left in place: $($_.Exception.Message)"
        }
    }
}

# --- Optionally stop processes --------------------------------------------

# Read the current process set once, so both branches below report from the
# same snapshot (and so the elseif branch has a value to test).
$procs = @(Get-MemgwRunningProcesses)

if ($StopProcesses) {
    if ($procs.Count -eq 0) {
        Write-Host '  no pilot processes running'
    }
    foreach ($p in $procs) {
        $label = if ($p.Kind -eq 'gateway') { 'gateway' } else { "collector:$($p.Name)" }
        if ($PSCmdlet.ShouldProcess("pid $($p.ProcId) $label", 'stop process')) {
            try {
                Stop-Process -Id $p.ProcId -Force -ErrorAction Stop
                Write-MemgwLog -Name 'ops' -Message "uninstall: stopped $label pid=$($p.ProcId)"
                Write-Host "  stopped $label pid=$($p.ProcId)"
            } catch {
                Write-Host "  could not stop $label pid=$($p.ProcId): $($_.Exception.Message)"
            }
        }
    }
} elseif ($procs.Count -gt 0) {
    Write-Host ''
    Write-Host "  note: $($procs.Count) pilot process(es) still running."
    Write-Host '  Re-run with -StopProcesses to stop them. They will not return after the next logon.'
}

# --- Clean up our own marker (keep the manifest as a record) ---------------

if (Test-Path -LiteralPath $stopFile) {
    # The supervisor polls on an interval, so it may not have read the stop file
    # yet. Wait for the supervisor process itself to disappear rather than
    # sleeping a fixed time -- a fixed sleep races the poll and can delete the
    # marker before it is seen, leaving a supervisor running with no task.
    $deadline = (Get-Date).AddSeconds(30)
    $stillSupervising = $true
    while ((Get-Date) -lt $deadline) {
        $sup = Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
               Where-Object { $_.CommandLine -match '-File.*memgw-supervisor\.ps1' }
        if (-not $sup) { $stillSupervising = $false; break }
        Start-Sleep -Milliseconds 500
    }

    if ($stillSupervising) {
        Write-Host '  WARNING: a supervisor is still running after 30s.'
        Write-Host "  Its stop file is left in place so it exits at its next poll: $stopFile"
        Write-Host '  Check for it with: status-memgw-persistence.ps1'
    } elseif ($PSCmdlet.ShouldProcess($stopFile, 'remove stale stop file')) {
        Remove-Item -LiteralPath $stopFile -Force
        Write-Host "  removed stop file: $stopFile"
    }
}

Write-Host ''
Write-Host 'Uninstall complete. Spools, logs, secrets and the database were not touched.'
