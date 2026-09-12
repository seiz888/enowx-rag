<#
.SYNOPSIS
    Install the memgw pilot as Scheduled Tasks that survive logoff/reboot.

.DESCRIPTION
    Creates one Scheduled Task, run at logon under the current user:

        \memgw\supervisor

    which starts the gateway, waits for it to answer, starts the per-principal
    collectors, and then keeps them alive (see memgw-supervisor.ps1).

    Why one task and not three
    --------------------------
    The gateway must be ready before a collector accepts a hook. Three
    independent tasks would race that ordering on every logon. One supervisor
    task owns the order once, in one place, where it can be read.

    Why no secrets in the task
    --------------------------
    The task action runs a script by path with no arguments. The gateway's DSN
    is composed inside the runspace from D:\memgw\secrets\pg_app, and each
    collector names its credential with --token-file. Nothing sensitive is in
    the action, the arguments, the Task Scheduler history, or any process
    command line.

    Reversibility
    -------------
    Every created task is recorded in D:\memgw\run\installed-tasks.json with a
    hash of its action. uninstall-memgw-persistence.ps1 reads that manifest and
    removes exactly those tasks. Pre-existing configuration is backed up before
    this script changes anything, and this script creates no task it does not
    record.

.PARAMETER WhatIf
    Report what would happen without creating anything.

.NOTES
    Read-only with respect to agent settings and to checkpoint/resolver code.
    This script does not touch ~/.claude, ~/.omp, or any repository source.
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    # Do not start the stack now; only register the tasks.
    [switch]$NoStart,
    # Override the command used for the immediate verification run.
    [switch]$SkipVerify
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'Memgw.Common.ps1')

$taskPath = '\memgw\'
$taskName = 'supervisor'
$fullName = "$taskPath$taskName"
$manifestPath = Join-Path $script:MemgwRoot 'run\installed-tasks.json'
$backupDir = Join-Path $script:MemgwRoot ("backups\ops-" + (Get-Date).ToString('yyyyMMdd-HHmmss'))

function Assert-NotElevatedArtifact {
    <#
    .SYNOPSIS
        Refuse to run against a task path that already exists with our name.
    .DESCRIPTION
        A task of this name from another source would be silently replaced. The
        operator is told to uninstall first, so the replacement is a decision
        rather than a surprise.
    #>
    $existing = Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName -ErrorAction SilentlyContinue
    if ($existing) {
        throw "task $fullName already exists. Run uninstall-memgw-persistence.ps1 first, or remove it deliberately."
    }
}

Write-Host 'memgw persistence install'
Write-Host "  binary   : $script:MemgwBin"
Write-Host "  logs     : $script:MemgwLogs"
Write-Host "  manifest : $manifestPath"
Write-Host ''

# --- Preconditions ---------------------------------------------------------

if (-not (Test-Path -LiteralPath $script:MemgwBin)) { throw "binary not found: $script:MemgwBin" }
foreach ($s in @('pg_app', 'claude.token', 'omp.token')) {
    $p = Join-Path $script:MemgwSecrets $s
    if (-not (Test-Path -LiteralPath $p)) { throw "required secret missing: $p" }
}

Assert-NotElevatedArtifact
Initialize-MemgwLogs

# --- Backup ----------------------------------------------------------------
# Record the pre-existing task state so a reviewer can see exactly what was
# there before. There is nothing of ours to merge with (we refuse if the name
# exists), but a manifest of the before-state is what makes the claim checkable.

New-Item -ItemType Directory -Force -Path $backupDir | Out-Null
# Force an array and serialise an explicit object: ConvertTo-Json over an empty
# pipeline emits nothing at all, which leaves a zero-byte file that looks like a
# successful backup. A written snapshot that says "there were none" is a
# different claim from an absent one, and the difference matters when the
# backup is the evidence.
$before = @(
    Get-ScheduledTask -ErrorAction SilentlyContinue |
        Where-Object { $_.TaskPath -like '*memgw*' } |
        Select-Object TaskName, TaskPath, State
)
$snapshot = [ordered]@{
    taken_at       = (Get-Date).ToString('o')
    taken_by       = "$env:USERDOMAIN\$env:USERNAME"
    task_path      = $taskPath
    existing_tasks = $before
}
$snapshot | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath (Join-Path $backupDir 'tasks-before.json') -Encoding utf8
Write-MemgwLog -Name 'ops' -Message "install: backed up pre-existing memgw task state to $backupDir"

# --- Build the action ------------------------------------------------------

$supervisor = Join-Path $PSScriptRoot 'memgw-supervisor.ps1'

# Windowless action; see New-MemgwHiddenScriptAction. The supervisor already starts
# its own children with Start-MemgwHidden (CreateNoWindow), so no popup ever
# came from the supervised stack -- only from this task's own action, which
# created a console for powershell.exe at every logon.
# -NoProfile so a user profile cannot alter behaviour at logon; -ExecutionPolicy
# Bypass because the script is unsigned and lives on a local fixed disk (both
# are supplied by New-MemgwHiddenScriptAction).
$action = New-MemgwHiddenScriptAction -ScriptPath $supervisor

# At logon of the installing user: the same account that owns the spool, the
# token files and the agent sessions. No stored password, no SYSTEM, no
# elevation -- the service runs with exactly the rights an interactive start
# would have.
$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 1)

$principal = New-ScheduledTaskPrincipal `
    -UserId "$env:USERDOMAIN\$env:USERNAME" `
    -LogonType Interactive `
    -RunLevel Limited

if ($WhatIfPreference) {
    Write-Host 'WhatIf: would register'
    Write-Host "  task    : $fullName"
    Write-Host "  execute : $($action.Execute)"
    Write-Host "  args    : $($action.Arguments)"
    Write-Host '  trigger : at logon'
    Write-Host ''
    Write-Host 'No changes made.'
    exit 0
}

# --- Register --------------------------------------------------------------

Register-ScheduledTask `
    -TaskPath $taskPath `
    -TaskName $taskName `
    -Action $action `
    -Trigger $trigger `
    -Settings $settings `
    -Principal $principal `
    -Description 'memgw pilot: start gateway then collectors at logon, keep them alive.' | Out-Null

Write-MemgwLog -Name 'ops' -Message "install: registered $fullName"

# --- Manifest --------------------------------------------------------------
# The uninstaller removes exactly what is listed here, and refuses to run if the
# recorded action hash no longer matches (someone edited the task by hand).

$registered = Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName
$manifest = [ordered]@{
    installed_at = (Get-Date).ToString('o')
    installed_by = "$env:USERDOMAIN\$env:USERNAME"
    task_path    = $taskPath
    task_name    = $taskName
    action_hash  = Get-MemgwTaskHash -Task $registered
    supervisor   = $supervisor
    binary       = $script:MemgwBin
    note         = 'Remove with uninstall-memgw-persistence.ps1. Do not edit by hand; the hash will fail.'
}
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $manifestPath) | Out-Null
$manifest | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $manifestPath -Encoding utf8
Write-MemgwLog -Name 'ops' -Message 'install: wrote task manifest'

Write-Host ''
Write-Host "registered: $fullName"
Write-Host "manifest  : $manifestPath"
Write-Host "backup    : $backupDir"
Write-Host ''

# --- Verification ----------------------------------------------------------
# Prove the task actually works, without rebooting: run the supervisor in
# one-shot mode via the same script and interpreter the task will use, and
# confirm readiness. This exercises the real path, not a summary of it.

if ($SkipVerify) {
    Write-Host 'verification skipped (-SkipVerify)'
    exit 0
}

Write-Host 'verifying: starting supervisor one-shot...'
$verifyExit = Start-MemgwHiddenScript -ScriptPath $supervisor -ExtraArgs @('-Once')

if ($verifyExit -ne 0) {
    Write-MemgwLog -Name 'ops' -Level 'error' -Message "install: one-shot verification failed exit=$verifyExit"
    throw "one-shot verification failed (exit $verifyExit); see $script:MemgwLogs\ops.log"
}
Write-MemgwLog -Name 'ops' -Message 'install: one-shot verification OK'
Write-Host 'verification OK: gateway answered and collector pipes opened'
