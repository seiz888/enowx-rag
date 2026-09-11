<#
.SYNOPSIS
    Supervise the memgw gateway and collectors: start, watch, restart.

.DESCRIPTION
    Task Scheduler can run a program at logon but has no reliable
    restart-on-failure for a long-lived foreground process. This supervisor is
    that missing piece.

    It is deliberately small and boring:

      1. Start the gateway, wait until it answers, and only then start the
         collectors. A collector whose gateway is not yet up would spool an
         event that has nowhere to go, which is the failure this ordering
         exists to prevent.
      2. Poll every few seconds. If any supervised process has exited, restart
         it -- the gateway first, with a fresh readiness wait, then its
         collectors.
      3. Write one line per event to D:\memgw\logs. Never a secret, never a DSN.

    It is meant to run at logon under the current user, which is the same
    account the agents and the spool already belong to. It needs no elevation:
    it starts the same binary with the same arguments an operator would type.

    Stopping is a file, not a signal: create D:\memgw\run\supervisor.stop and
    the loop exits after its current sleep, leaving child processes running
    (they are independent processes, not job-object children). The uninstall
    script removes that file again.

.PARAMETER Once
    Start everything, verify readiness, report, and exit without supervising.
    Used by install verification, which must not leave a watcher behind.

.NOTES
    Read-only with respect to checkpoint and resolver code: this file starts
    the existing binaries and nothing else.
#>

[CmdletBinding()]
param(
    # Start and verify, then exit. The installer uses this to prove the tasks work.
    [switch]$Once,
    # Seconds between health polls in supervised mode.
    [int]$PollSeconds = 5
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'Memgw.Common.ps1')

$stopFile = Join-Path $script:MemgwRoot 'run\supervisor.stop'
$runDir   = Join-Path $script:MemgwRoot 'run'
if (-not (Test-Path -LiteralPath $runDir)) { New-Item -ItemType Directory -Force -Path $runDir | Out-Null }
Initialize-MemgwLogs

function Get-Supervised {
    <#
    .SYNOPSIS
        Current pilot processes, keyed by a stable identity.
    .DESCRIPTION
        Identity is "<kind>:<name>" -- gateway, collector:claude, collector:omp.
        Matching on identity rather than pid means a process that replaced a
        dead one is recognised as present rather than as a second instance.
    #>
    $map = @{}
    foreach ($p in Get-MemgwRunningProcesses) {
        $key = if ($p.Kind -eq 'gateway') { 'gateway' } else { "collector:$($p.Name)" }
        $map[$key] = $p
    }
    return $map
}

function Start-AllVerified {
    <#
    .SYNOPSIS
        Bring up gateway then collectors, each verified ready.
    .DESCRIPTION
        Returns $true only when the gateway answers and every collector pipe
        exists. A partial start is reported as failure, because a collector
        with no gateway is worse than a clear error.
    #>
    [CmdletBinding()]
    param()

    $existing = Get-Supervised

    # --- Gateway first -----------------------------------------------------
    if ($existing.ContainsKey('gateway')) {
        Write-MemgwLog -Name 'gateway' -Message "already running pid=$($existing['gateway'].ProcId)"
    } else {
        [void](Start-MemgwGateway)
    }

    if (-not (Test-MemgwGatewayReady -TimeoutSeconds 30)) {
        Write-MemgwLog -Name 'gateway' -Level 'error' -Message 'gateway did not become ready within 30s'
        return $false
    }
    Write-MemgwLog -Name 'gateway' -Message "ready at $script:MemgwGatewayAddr"

    # --- Collectors only now ----------------------------------------------
    $ok = $true
    foreach ($c in $script:MemgwCollectors) {
        $key = "collector:$($c.Name)"
        if ($existing.ContainsKey($key)) {
            Write-MemgwLog -Name $c.Name -Message "already running pid=$($existing[$key].ProcId)"
        } else {
            [void](Start-MemgwCollector -Collector $c)
        }
        if (-not (Test-MemgwCollectorPipeReady -Pipe $c.Pipe -TimeoutSeconds 20)) {
            Write-MemgwLog -Name $c.Name -Level 'error' -Message "pipe did not appear: $($c.Pipe)"
            $ok = $false
        } else {
            Write-MemgwLog -Name $c.Name -Message "pipe ready: $($c.Pipe)"
        }
    }
    return $ok
}

# --- Entry point -----------------------------------------------------------

if ($Once) {
    Write-MemgwLog -Name 'ops' -Message 'supervisor: one-shot start'
    $ok = Start-AllVerified
    if ($ok) { Write-MemgwLog -Name 'ops' -Message 'supervisor: one-shot OK'; exit 0 }
    Write-MemgwLog -Name 'ops' -Level 'error' -Message 'supervisor: one-shot FAILED'
    exit 1
}

Write-MemgwLog -Name 'ops' -Message "supervisor: entering supervised mode (poll ${PollSeconds}s, stop file $stopFile)"

# Remove a stale stop file from a previous uninstall so a fresh install is not
# immediately stopped by an earlier run's marker.
if (Test-Path -LiteralPath $stopFile) { Remove-Item -LiteralPath $stopFile -Force }

[void](Start-AllVerified)

while (-not (Test-Path -LiteralPath $stopFile)) {
    Start-Sleep -Seconds $PollSeconds
    try {
        $running = Get-Supervised
        if (-not $running.ContainsKey('gateway')) {
            Write-MemgwLog -Name 'gateway' -Level 'warn' -Message 'gateway is gone; restarting entire stack in order'
            [void](Start-AllVerified)
            continue
        }
        # If the gateway is alive, only replace the collectors that vanished.
        foreach ($c in $script:MemgwCollectors) {
            if (-not $running.ContainsKey("collector:$($c.Name)")) {
                Write-MemgwLog -Name $c.Name -Level 'warn' -Message 'collector is gone; restarting'
                [void](Start-MemgwCollector -Collector $c)
                if (-not (Test-MemgwCollectorPipeReady -Pipe $c.Pipe -TimeoutSeconds 20)) {
                    Write-MemgwLog -Name $c.Name -Level 'error' -Message "restarted collector did not open its pipe: $($c.Pipe)"
                }
            }
        }
    } catch {
        # A supervisor that dies on a transient WMI error supervises nothing.
        Write-MemgwLog -Name 'ops' -Level 'warn' -Message "poll error: $($_.Exception.Message)"
    }
}

Write-MemgwLog -Name 'ops' -Message 'supervisor: stop file seen; exiting (children left running)'
exit 0
