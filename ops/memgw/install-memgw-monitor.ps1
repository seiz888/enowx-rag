<#
.SYNOPSIS
    Install the memgw health check as a Scheduled Task.

.DESCRIPTION
    Creates one Scheduled Task:

        \memgw\healthcheck

    which runs check-memgw-health.ps1 every 5 minutes and on logon. The task
    fails (non-zero exit) when a threshold is crossed, so the Task Scheduler
    "Last Run Result" is itself the alert -- an operator does not have to read
    a log to notice.

    Why a second task and not a second action on \memgw\supervisor
    --------------------------------------------------------------
    The supervisor's job is to keep the stack alive; its action must never fail
    for a monitoring reason. A separate task keeps a failed health check from
    restarting the gateway, and keeps the gateway's restart policy from masking
    a health regression. They are recorded in separate manifests so neither
    installer can remove the other's task.

    Why no secrets in the task
    --------------------------
    The health check reads no credential: the local probe sends no token, the
    remote probe deliberately sends none, and the database is reached through
    the running container. Nothing sensitive is in the action or its arguments.

.PARAMETER WhatIf
    Report what would happen without creating anything.

.NOTES
    Read-only with respect to agent settings and repository source. Does not
    touch the protected seizrag-qdrant container.
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    # Do not run the verification check now; only register the task.
    [switch]$SkipVerify
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:MemgwRoot = 'D:\memgw'
$taskPath = '\memgw\'
$taskName = 'healthcheck'
$fullName = "$taskPath$taskName"
$manifestPath = Join-Path $script:MemgwRoot 'run\installed-monitor.json'
$logDir = Join-Path $script:MemgwRoot 'logs'

function Get-MemgwTaskActionHash {
    param($Task)
    $a = $Task.Actions[0]
    $s = "$($a.Execute)|$($a.Arguments)|$($a.WorkingDirectory)"
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($s)
        return ([BitConverter]::ToString($sha.ComputeHash($bytes)) -replace '-', '').ToLowerInvariant()
    } finally { $sha.Dispose() }
}

$existing = Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName -ErrorAction SilentlyContinue
if ($existing) {
    throw "task $fullName already exists. Remove it deliberately first, or run the uninstall."
}

$checkScript = Join-Path $PSScriptRoot 'check-memgw-health.ps1'
if (-not (Test-Path -LiteralPath $checkScript)) { throw "health check script not found: $checkScript" }

New-Item -ItemType Directory -Force -Path $logDir | Out-Null

$powershell = (Get-Command powershell.exe).Source
$action = New-ScheduledTaskAction `
    -Execute $powershell `
    -Argument ('-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "{0}"' -f $checkScript) `
    -WorkingDirectory $script:MemgwRoot

# Every 5 minutes, indefinitely, plus at logon so a fresh boot is covered
# before the first interval elapses.
# The repetition duration must be positive and finite: both TimeSpan::MaxValue
# and TimeSpan::Zero serialise to duration strings the Task Scheduler XML schema
# rejects. Ten years is finite and far beyond this pilot's horizon.
$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 5) -RepetitionDuration (New-TimeSpan -Days 3650)
$logonTrigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"

$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 5) `
    -RestartCount 0

$principal = New-ScheduledTaskPrincipal `
    -UserId "$env:USERDOMAIN\$env:USERNAME" `
    -LogonType Interactive `
    -RunLevel Limited

if ($WhatIfPreference) {
    Write-Host 'WhatIf: would register'
    Write-Host "  task    : $fullName"
    Write-Host "  args    : $($action.Arguments)"
    Write-Host '  trigger : every 5 minutes + at logon'
    exit 0
}

Register-ScheduledTask `
    -TaskPath $taskPath `
    -TaskName $taskName `
    -Action $action `
    -Trigger @($trigger, $logonTrigger) `
    -Settings $settings `
    -Principal $principal `
    -Description 'memgw pilot: health check (queue age, dead letters, projection lag, remote service).' | Out-Null

$registered = Get-ScheduledTask -TaskPath $taskPath -TaskName $taskName
$manifest = [ordered]@{
    installed_at = (Get-Date).ToString('o')
    installed_by = "$env:USERDOMAIN\$env:USERNAME"
    task_path    = $taskPath
    task_name    = $taskName
    action_hash  = Get-MemgwTaskActionHash -Task $registered
    script       = $checkScript
    note         = 'Remove with uninstall-memgw-monitor.ps1. Do not edit by hand; the hash will fail.'
}
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $manifestPath) | Out-Null
$manifest | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $manifestPath -Encoding utf8

Write-Host "registered: $fullName"
Write-Host "manifest  : $manifestPath"

if ($SkipVerify) { exit 0 }

Write-Host 'verifying: running the check once...'
$verify = Start-Process -FilePath $powershell `
    -ArgumentList @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $checkScript) `
    -PassThru -Wait -WindowStyle Hidden
Write-Host "verification exit: $($verify.ExitCode) (0=ok, 1=warn, 2=fail)"
if ($verify.ExitCode -eq 2) {
    throw 'the health check reports a failing state; fix it before relying on the task'
}
Write-Host 'verification OK'
