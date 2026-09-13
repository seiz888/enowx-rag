<#
.SYNOPSIS
    Install the memgw pilot as Scheduled Tasks that survive logoff/reboot.

.DESCRIPTION
    Creates one Scheduled Task, run at logon under the current user:

        \memgw\supervisor

    which starts the per-principal collectors and keeps them alive (see
    memgw-supervisor.ps1). It runs in COLLECTORS-ONLY mode: the collectors
    forward to the authoritative gateway named in Memgw.Common.ps1 (the
    remote VPS), so this workstation no longer starts or depends on a local
    gateway or a local ledger.

    Why collectors-only is the ordinary install
    -------------------------------------------
    The ledger's authority moved off this workstation. A supervisor that also
    started a local gateway would resurrect a second writer against a stopped
    local database, and every hook would then have two possible destinations
    with only one of them correct. The ordinary install therefore supervises
    exactly the collectors -- the only local component that still has a job.
    Bringing the local stack up is a deliberate rollback action, not an
    installer side effect; see D:\memgw\run\cutover-state.txt.

    Why one task and not three
    --------------------------
    Three independent tasks would race their own startup and any of them could
    be the one that is missing after a reboot. One supervisor task owns the
    set once, in one place, where it can be read.

    Why no secrets in the task
    --------------------------
    The task action runs a script by path plus a fixed mode flag. Each
    collector names its credential with --token-file, and the collector reads
    the secret itself. Nothing sensitive is in the action, the arguments, the
    Task Scheduler history, or any process command line.

    Notably there is NO local database secret here any more: this install does
    not compose a DSN, because it does not start anything that needs one. That
    dependency is what made a client install fail on a machine whose local
    ledger had been retired.

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

# The mode this task runs in permanently. Named once so the registered argv and
# the verification run below cannot drift apart.
$supervisorArgs = @('-CollectorsOnly')

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
# Per-host collector credentials only. pg_app (the local ledger password) is
# deliberately absent: this install starts no local gateway, so requiring it
# would block installation on a correctly-migrated client.
foreach ($s in @('claude.token', 'omp.token', 'codex.token', 'droid.token', 'opencode.token')) {
    $p = Join-Path $script:MemgwSecrets $s
    if (-not (Test-Path -LiteralPath $p)) { throw "required collector credential missing: $p" }
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
# -CollectorsOnly is part of the installed action, permanently. This is the
# registered shape now, not a stopgap: the ledger's authority is remote.
$action = New-MemgwHiddenScriptAction -ScriptPath $supervisor -ExtraArgs $supervisorArgs

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
    -Description 'memgw: keep the per-host collectors alive at logon; forward to the remote gateway.' | Out-Null

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
    supervisor_args = $supervisorArgs
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
# one-shot mode via the same script, interpreter AND mode flag the task will
# use, and confirm readiness. Verifying a different mode than the one
# registered would prove nothing about the installed task -- the previous
# version of this check ran -Once WITHOUT -CollectorsOnly, so it exercised the
# gateway+collectors path and would have passed while the installed
# collectors-only task did something else entirely.

if ($SkipVerify) {
    Write-Host 'verification skipped (-SkipVerify)'
    exit 0
}

Write-Host 'verifying: starting supervisor one-shot...'
$verifyExit = Start-MemgwHiddenScript -ScriptPath $supervisor -ExtraArgs ($supervisorArgs + @('-Once'))

if ($verifyExit -ne 0) {
    Write-MemgwLog -Name 'ops' -Level 'error' -Message "install: one-shot verification failed exit=$verifyExit"
    throw "one-shot verification failed (exit $verifyExit); see $script:MemgwLogs\ops.log"
}
Write-MemgwLog -Name 'ops' -Message 'install: one-shot verification OK'
Write-Host 'verification OK: every collector pipe opened (collectors-only mode)'
