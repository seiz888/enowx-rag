<#
.SYNOPSIS
    Shared launch helpers for the memgw pilot services.

.DESCRIPTION
    One place that knows where the memgw assets live, how to build an
    environment block that contains no secret on any command line, and how to
    probe readiness.

    Why this file exists
    --------------------
    The gateway's connection string carries the database password. PostgreSQL
    takes it only through MEMGW_DSN; the binary has no --dsn flag and refuses to
    guess a target. So the value has to reach the process as an environment
    variable -- and an environment variable set inside this script never appears
    in a command line, a Scheduled Task action, or the Task Scheduler history.

    The alternative -- a wrapper that echoes the DSN into a child process via
    an argument -- would put the password in the process list, where every
    account on the machine can read it. This file exists so that never happens.

    Nothing here prints a secret. Logs record the redacted DSN (host/database
    only), never the credential.
#>

Set-StrictMode -Version Latest

# --- Locations -------------------------------------------------------------
# Everything is D:-based: the spool and logs must not fill C:, which is the
# disk this workstation has historically run out of.

$script:MemgwRoot   = 'D:\memgw'
$script:MemgwBin    = Join-Path $script:MemgwRoot 'bin\enowx-rag.exe'
$script:MemgwSecrets = Join-Path $script:MemgwRoot 'secrets'
$script:MemgwLogs   = Join-Path $script:MemgwRoot 'logs'
$script:MemgwOps    = $PSScriptRoot

# The collector spools, one directory per host.
#
# All five hosts have a collector: a lifecycle event that fires while the
# gateway is unreachable is held in that host's encrypted spool and forwarded
# when it returns. Until the migration, only claude and omp had one, so codex,
# droid and opencode submitted straight to the gateway and an event fired
# during an outage was simply lost. A cutover window is exactly such an outage,
# which is why these three are added before the clients move rather than after.
#
# Per-host isolation is the point: each collector has its own spool directory,
# its own named pipe and its own principal token, so one host's queue can never
# be drained under another host's identity.
$script:MemgwCollectors = @(
    @{ Name = 'claude';   Dir = 'D:\memgw\collector\claude';   Pipe = '\\.\pipe\memgw-collector-claude';   Token = 'D:\memgw\secrets\claude.token' }
    @{ Name = 'omp';      Dir = 'D:\memgw\collector\omp';      Pipe = '\\.\pipe\memgw-collector-omp';      Token = 'D:\memgw\secrets\omp.token' }
    @{ Name = 'codex';    Dir = 'D:\memgw\collector\codex';    Pipe = '\\.\pipe\memgw-collector-codex';    Token = 'D:\memgw\secrets\codex.token' }
    @{ Name = 'droid';    Dir = 'D:\memgw\collector\droid';    Pipe = '\\.\pipe\memgw-collector-droid';    Token = 'D:\memgw\secrets\droid.token' }
    @{ Name = 'opencode'; Dir = 'D:\memgw\collector\opencode'; Pipe = '\\.\pipe\memgw-collector-opencode'; Token = 'D:\memgw\secrets\opencode.token' }
)

# Where the collectors send. This is the single client-side cutover point: it was
# the workstation gateway on loopback, and after the migration it is the VPS. The
# value is an ORIGIN with no path -- the binary appends /memgw/v1/... itself, so
# including /memgw here would produce /memgw/memgw/v1/... and every submit would
# fail as a route miss.
$script:MemgwGatewayUrl = 'https://rag.seiz.cloud'

# The retired local gateway. Only the gateway+collectors path uses these, and that
# path is no longer the registered one (the supervisor runs -CollectorsOnly because
# the VPS is now authoritative for the ledger). Kept so a rollback can still bring
# the local stack up, and kept distinct from the URL above so that "where collectors
# send" can never silently become "where a local gateway is probed".
$script:MemgwLocalGatewayAddr = '127.0.0.1:7791'
$script:MemgwLocalGatewayUrl  = 'http://127.0.0.1:7791'

# --- Logging ---------------------------------------------------------------

function Initialize-MemgwLogs {
    <#
    .SYNOPSIS
        Create the log and spool directories, and tighten the log directory.
    .DESCRIPTION
        Idempotent. Logs must never be world-writable: they are the record an
        operator reads after an incident.
    #>
    foreach ($d in @($script:MemgwLogs, (Join-Path $script:MemgwLogs 'gateway'), (Join-Path $script:MemgwLogs 'collectors'))) {
        if (-not (Test-Path -LiteralPath $d)) {
            New-Item -ItemType Directory -Force -Path $d | Out-Null
        }
    }
    # Remove inherited broad ACEs from the log tree; leave Administrators and
    # SYSTEM, and the current user. A log an unprivileged account can rewrite is
    # not evidence.
    try {
        $me = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
        & icacls $script:MemgwLogs /inheritance:r /grant:r "${me}:(OI)(CI)F" 'BUILTIN\Administrators:(OI)(CI)F' 'NT AUTHORITY\SYSTEM:(OI)(CI)F' 2>&1 | Out-Null
    } catch {
        # ACL tightening is best-effort here; a failure is reported, not fatal,
        # because the service still works and the operator can fix perms.
        Write-MemgwLog -Name 'ops' -Level 'warn' -Message "could not tighten log ACLs: $($_.Exception.Message)"
    }
}

function Write-MemgwLog {
    <#
    .SYNOPSIS
        Append one line to an operations log.
    .PARAMETER Name
        Log stream: 'ops', 'gateway', or a collector name.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][string]$Message,
        [ValidateSet('info', 'warn', 'error')][string]$Level = 'info'
    )
    $dir = if ($Name -eq 'gateway') { Join-Path $script:MemgwLogs 'gateway' }
           elseif ($Name -eq 'ops') { $script:MemgwLogs }
           else { Join-Path $script:MemgwLogs 'collectors' }
    if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    $stamp = (Get-Date).ToString('yyyy-MM-ddTHH:mm:ss.fffzzz')
    $line  = "$stamp [$Level] $Message"
    Add-Content -LiteralPath (Join-Path $dir "$Name.log") -Value $line -Encoding utf8
}

# --- Secret handling -------------------------------------------------------

function Read-MemgwSecret {
    <#
    .SYNOPSIS
        Read a secret file and return its trimmed value.
    .DESCRIPTION
        The value is returned to the caller only. It is never logged, echoed,
        or placed in a command line by anything in this module.
    #>
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path)) {
        throw "memgw secret file not found: $Path"
    }
    $v = (Get-Content -LiteralPath $Path -Raw).Trim()
    if ([string]::IsNullOrWhiteSpace($v)) {
        throw "memgw secret file is empty: $Path"
    }
    return $v
}

function New-MemgwGatewayEnvironment {
    <#
    .SYNOPSIS
        Build the environment block the gateway needs, with no secret exposed.
    .DESCRIPTION
        Mirrors D:\memgw\env-serve.sh, but constructs the DSN in-process from the
        password file rather than shelling out with it. Returns a hashtable
        suitable for ProcessStartInfo.EnvironmentVariables.

        Least privilege is preserved exactly as env-serve.sh sets it: the serving
        role owns nothing and may only read and write rows in the managed schema.
    #>
    [CmdletBinding()]
    param()

    $pw = Read-MemgwSecret -Path (Join-Path $script:MemgwSecrets 'pg_app')
    $dsn = "postgres://memgw_app:${pw}@127.0.0.1:55440/memgw?sslmode=verify-full&sslrootcert=D:\memgw\certs\ca.crt"

    return @{
        MEMGW_DSN                      = $dsn
        MEMGW_SCHEMA                   = 'memgw'
        MEMGW_ENV                      = 'production'
        MEMGW_PRODUCTION_DATABASE      = 'memgw'
        MEMGW_PRODUCTION_SCHEMA        = 'memgw'
        MEMGW_PRODUCTION_ROLE          = 'memgw_app'
        MEMGW_PRODUCTION_DEPLOYMENT    = 'memgw-live-workstation-1'
        MEMGW_PRODUCTION_ALLOW         = 'serve'
    }
}

function Get-MemgwRedactedDsn {
    <#
    .SYNOPSIS
        The host/database of the gateway DSN, for logs and status output.
    #>
    # Written out rather than parsed from the built DSN, so reading status never
    # requires touching the password file at all.
    return '127.0.0.1:55440/memgw'
}

# --- Readiness -------------------------------------------------------------

function Test-MemgwGatewayReady {
    <#
    .SYNOPSIS
        Probe the gateway until it answers, or time out.
    .DESCRIPTION
        The gateway requires a per-principal credential, so an unauthenticated
        request is expected to be refused with 401. That refusal is proof the
        HTTP listener is up and routing -- which is exactly what a readiness
        probe needs to know, and it needs no credential to learn it.

        Returns $true when the gateway answered at all.
    #>
    [CmdletBinding()]
    param(
        [int]$TimeoutSeconds = 30,
        [int]$IntervalMs = 250
    )
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            # Built on HttpWebRequest rather than Invoke-WebRequest because this
            # must run on Windows PowerShell 5.1, which has no
            # -SkipHttpErrorCheck: a 401 would become a terminating error and
            # the probe would read "not ready" for a gateway that is serving.
            # Probes the LOCAL gateway, because that is the one Start-MemgwGateway
            # starts and the one this readiness wait guards. The remote endpoint is
            # the collectors' destination and is not started by this process; asking
            # it to be ready here would make the local stack unstartable whenever the
            # VPS was slow, for a dependency it does not have.
            $req = [System.Net.HttpWebRequest]::Create("$script:MemgwLocalGatewayUrl/memgw/v1/health")
            $req.Method = 'GET'
            $req.Timeout = 3000
            $req.AllowAutoRedirect = $false
            try {
                $resp = $req.GetResponse()
                $resp.Close()
                return $true
            } catch [System.Net.WebException] {
                # A protocol-level response (401, 404, 500...) still proves the
                # listener is bound and answering, which is what readiness asks.
                if ($_.Exception.Response) { return $true }
                # No response at all: connection refused / not yet listening.
            }
        } catch {
            # Anything unexpected: keep waiting until the deadline.
        }
        Start-Sleep -Milliseconds $IntervalMs
    }
    return $false
}

function Test-MemgwCollectorPipeReady {
    <#
    .SYNOPSIS
        Return $true once the collector's named pipe exists.
    .DESCRIPTION
        A collector that has not opened its pipe cannot accept a hook yet, so
        "the process is running" is not the readiness condition -- the pipe is.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Pipe,
        [int]$TimeoutSeconds = 20
    )
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        if (Test-Path -LiteralPath $Pipe) { return $true }
        Start-Sleep -Milliseconds 250
    }
    return $false
}

# --- Process launching -----------------------------------------------------

function Start-MemgwHidden {
    <#
    .SYNOPSIS
        Start a memgw component detached, with an explicit environment.
    .DESCRIPTION
        Never uses a shell. Arguments are passed as an array, so no value the
        caller supplies can be re-parsed as shell syntax. Secrets travel only in
        the environment block, which is not visible in the process list.

        The child is started with a hidden window and is not a job object
        child of this script: when the installer exits, the service keeps
        running, which is the entire point.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string[]]$ArgumentList,
        [hashtable]$Environment = @{},
        [string]$WorkingDirectory = $script:MemgwRoot
    )

    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName               = $script:MemgwBin
    $psi.WorkingDirectory       = $WorkingDirectory
    $psi.UseShellExecute        = $false   # required for EnvironmentVariables to apply
    $psi.CreateNoWindow         = $true
    $psi.RedirectStandardOutput = $false
    $psi.RedirectStandardError  = $false

    # Windows PowerShell 5.1 runs on .NET Framework, whose ProcessStartInfo has
    # no ArgumentList collection -- only the single Arguments string. The
    # arguments are therefore quoted here rather than passed as an array. That
    # is safe because every value this module passes is a fixed literal or a
    # filesystem path chosen by this script; no caller-supplied or
    # host-supplied text reaches this string.
    $psi.Arguments = ($ArgumentList | ForEach-Object { ConvertTo-MemgwArg -Value $_ }) -join ' '
    foreach ($k in $Environment.Keys) { $psi.EnvironmentVariables[$k] = [string]$Environment[$k] }

    $p = [System.Diagnostics.Process]::Start($psi)
    return $p
}

function ConvertTo-MemgwArg {
    <#
    .SYNOPSIS
        Quote one argument for the Windows command line.
    .DESCRIPTION
        Follows the CommandLineToArgvW rules the Go runtime uses: wrap in
        double quotes and escape embedded quotes and trailing backslashes.
    #>
    [CmdletBinding()]
    param([Parameter(Mandatory)][AllowEmptyString()][string]$Value)

    if ($Value -ne '' -and $Value -notmatch '[\s"]') { return $Value }

    $sb = New-Object System.Text.StringBuilder
    [void]$sb.Append('"')
    $backslashes = 0
    foreach ($ch in $Value.ToCharArray()) {
        if ($ch -eq '\') { $backslashes++; continue }
        if ($ch -eq '"') {
            [void]$sb.Append('\' * ($backslashes * 2 + 1))
            [void]$sb.Append('"')
            $backslashes = 0
            continue
        }
        if ($backslashes -gt 0) { [void]$sb.Append('\' * $backslashes); $backslashes = 0 }
        [void]$sb.Append($ch)
    }
    if ($backslashes -gt 0) { [void]$sb.Append('\' * ($backslashes * 2)) }
    [void]$sb.Append('"')
    return $sb.ToString()
}

function Start-MemgwGateway {
    <#
    .SYNOPSIS
        Start the gateway detached, returning the process.
    #>
    [CmdletBinding()]
    param()
    Initialize-MemgwLogs
    $env = New-MemgwGatewayEnvironment
    $p = Start-MemgwHidden -ArgumentList @('memgw', 'serve', '--addr', $script:MemgwLocalGatewayAddr) -Environment $env
    Write-MemgwLog -Name 'gateway' -Level 'info' -Message "started pid=$($p.Id) addr=$script:MemgwLocalGatewayAddr dsn=$(Get-MemgwRedactedDsn)"
    return $p
}

function Start-MemgwCollector {
    <#
    .SYNOPSIS
        Start one collector detached, returning the process.
    .PARAMETER Collector
        One of the hashtables in $script:MemgwCollectors.
    #>
    [CmdletBinding()]
    param([Parameter(Mandatory)][hashtable]$Collector)

    Initialize-MemgwLogs
    $args = @(
        'memgw', 'collector', 'run',
        '--dir', $Collector.Dir,
        '--pipe', $Collector.Pipe,
        '--gateway', $script:MemgwGatewayUrl,
        '--token-file', $Collector.Token
    )
    # Paths only. The credential is named by --token-file and read by the
    # binary itself, so it never enters this process's environment or argv.
    $p = Start-MemgwHidden -ArgumentList $args
    Write-MemgwLog -Name $Collector.Name -Level 'info' -Message "collector started pid=$($p.Id) pipe=$($Collector.Pipe) spool=$($Collector.Dir) token-file=$($Collector.Token)"
    return $p
}

# --- Containers ------------------------------------------------------------
# The gateway is useless without its ledger, and the ledger lives in a Docker
# container on this machine. After a reboot the container is simply absent
# until something starts it: nothing in this pilot ever did, so the gateway
# came up, failed to reach its database, and every hook reported a durability
# failure. These helpers are that missing step.

$script:MemgwContainers = @(
    # The canonical ledger. If this is down the gateway cannot commit anything.
    @{ Name = 'memgw-live'; Required = $true;  ReadyProbe = 'ledger' }
    # The vector projection. Not required for durability -- the gateway writes
    # events and receipts with the projection absent and catches up later -- so
    # a failure here is reported and does not block startup.
    @{ Name = 'memgw-qdrant'; Required = $false; ReadyProbe = $null }
)

function Test-MemgwDockerReady {
    <#
    .SYNOPSIS
        Return $true once the Docker daemon answers.
    .DESCRIPTION
        After a reboot Docker Desktop is still starting when the logon task
        fires, so "no containers" usually means "the daemon is not up yet"
        rather than "the containers are gone". Waiting for the daemon first
        keeps that distinction out of the caller.
    #>
    [CmdletBinding()]
    param([int]$TimeoutSeconds = 180, [int]$IntervalMs = 2000)

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            $null = & docker info --format '{{.ServerVersion}}' 2>$null
            if ($LASTEXITCODE -eq 0) { return $true }
        } catch { }
        Start-Sleep -Milliseconds $IntervalMs
    }
    return $false
}

function Test-MemgwContainerRunning {
    <#
    .SYNOPSIS
        Return $true when the named container is running.
    #>
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$Name)
    try {
        $state = & docker inspect -f '{{.State.Running}}' $Name 2>$null
        return ($LASTEXITCODE -eq 0 -and "$state".Trim() -eq 'true')
    } catch {
        return $false
    }
}

function Test-MemgwLedgerReady {
    <#
    .SYNOPSIS
        Return $true once the ledger accepts a connection.
    .DESCRIPTION
        `docker start` returns as soon as the container is created, well before
        PostgreSQL has replayed its WAL and is accepting connections. The
        gateway would fail its first commit against a database that is seconds
        away from being up, so readiness is a real query, not a state flag.
    #>
    [CmdletBinding()]
    param([Parameter(Mandatory)][string]$Container, [int]$TimeoutSeconds = 120, [int]$IntervalMs = 1000)

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            $out = & docker exec -u postgres $Container pg_isready -q 2>$null
            if ($LASTEXITCODE -eq 0) { return $true }
        } catch { }
        Start-Sleep -Milliseconds $IntervalMs
    }
    return $false
}

function Start-MemgwContainer {
    <#
    .SYNOPSIS
        Ensure one container is running and ready.
    .DESCRIPTION
        Returns $true when the container is running (and, for the ledger,
        accepting connections). A container already running is left alone.
    #>
    [CmdletBinding()]
    param([Parameter(Mandatory)][hashtable]$Container, [int]$TimeoutSeconds = 120)

    $name = $Container.Name
    if (Test-MemgwContainerRunning -Name $name) {
        Write-MemgwLog -Name 'ops' -Message "container $name already running"
    } else {
        Write-MemgwLog -Name 'ops' -Message "starting container $name"
        $out = & docker start $name 2>&1
        if ($LASTEXITCODE -ne 0) {
            Write-MemgwLog -Name 'ops' -Level 'error' -Message "docker start $name failed: $($out -join ' ')"
            return $false
        }
    }

    if ($Container.ReadyProbe -eq 'ledger') {
        if (-not (Test-MemgwLedgerReady -Container $name -TimeoutSeconds $TimeoutSeconds)) {
            Write-MemgwLog -Name 'ops' -Level 'error' -Message "container $name is running but the ledger is not accepting connections"
            return $false
        }
    }
    return $true
}

function Start-MemgwContainers {
    <#
    .SYNOPSIS
        Bring up every required container, else report which failed.
    .DESCRIPTION
        A required container that cannot be brought up is a hard failure -- the
        caller must not start a gateway that has nowhere to write. An optional
        one is logged and ignored.
    #>
    [CmdletBinding()]
    param()

    if (-not (Test-MemgwDockerReady -TimeoutSeconds 180)) {
        Write-MemgwLog -Name 'ops' -Level 'error' -Message 'docker daemon did not answer within 180s'
        return $false
    }

    $ok = $true
    foreach ($c in $script:MemgwContainers) {
        $started = Start-MemgwContainer -Container $c
        if (-not $started -and $c.Required) {
            $ok = $false
        } elseif (-not $started) {
            Write-MemgwLog -Name 'ops' -Level 'warn' -Message "optional container $($c.Name) is not available"
        }
    }
    return $ok
}

# --- Discovery -------------------------------------------------------------

function Get-MemgwRunningProcesses {
    <#
    .SYNOPSIS
        Find memgw gateway and collector processes this pilot owns.
    .DESCRIPTION
        Matched on the resolved binary path plus the subcommand, so an
        unrelated process that merely mentions the word "memgw" is not
        returned. This is the list that status reports and that an operator may
        stop; nothing else is ever touched.
    #>
    [CmdletBinding()]
    param()

    $bin = $script:MemgwBin
    $procs = Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object {
        $_.ExecutablePath -eq $bin -and $_.CommandLine -match 'memgw\s+(serve|collector)'
    }

    foreach ($p in $procs) {
        $kind = if ($p.CommandLine -match 'memgw\s+serve') { 'gateway' } else { 'collector' }
        $name = ''
        if ($kind -eq 'collector' -and $p.CommandLine -match '--pipe\s+\\\\.\\pipe\\([^\s"]+)') {
            $name = $Matches[1] -replace '^memgw-collector-', ''
        }
        [PSCustomObject]@{
            Kind    = $kind
            Name    = $name
            ProcId  = $p.ProcessId
            PPID    = $p.ParentProcessId
            Started = $p.CreationDate
            Command = $p.CommandLine
        }
    }
}

function Get-MemgwCollectorStats {
    <#
    .SYNOPSIS
        Read a collector's queue counts.
    .DESCRIPTION
        Uses the binary's own reader against the spool directory. Reads no
        database and no credential.
    #>
    [CmdletBinding()]
    param([Parameter(Mandatory)][hashtable]$Collector)
    try {
        $raw = & $script:MemgwBin memgw collector stats --dir $Collector.Dir 2>&1 | Out-String
        return ($raw | ConvertFrom-Json)
    } catch {
        return $null
    }
}

function Start-MemgwHiddenScript {
    <#
    .SYNOPSIS
        Run a .ps1 windowless and return its exit code, for installer checks.
    .DESCRIPTION
        Same launcher the registered task uses, so the verification exercises
        the real path. Start-Process -Wait rather than the call operator:
        wscript.exe is GUI-subsystem and `&` neither waits for one nor sets
        $LASTEXITCODE.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ScriptPath,
        [string[]]$ExtraArgs = @()
    )

    if (-not (Test-Path -LiteralPath $ScriptPath)) {
        throw "script not found: $ScriptPath"
    }
    if ($ScriptPath -match '"' -or ($ExtraArgs | Where-Object { $_ -match '"' })) {
        throw 'a double quote is not supported in a task argument value'
    }

    $powershell = (Get-Command powershell.exe).Source
    $wscript = Join-Path $env:SystemRoot 'System32\wscript.exe'
    # Quoted here: Start-Process -ArgumentList joins its array with spaces and
    # does not quote for you, so an unquoted path with a space would be split
    # into two arguments and the run would fail with a meaningless code.
    $args = @(
        '//B', '//Nologo', (ConvertTo-MemgwArg -Value $script:MemgwSilentLauncher)
        (ConvertTo-MemgwArg -Value $powershell)
        '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass'
        '-WindowStyle', 'Hidden', '-File', (ConvertTo-MemgwArg -Value $ScriptPath)
    ) + ($ExtraArgs | ForEach-Object { ConvertTo-MemgwArg -Value $_ })

    $p = Start-Process -FilePath $wscript -ArgumentList $args -PassThru -Wait -WindowStyle Hidden
    return $p.ExitCode
}

# --- Task identity ---------------------------------------------------------

$script:MemgwSilentLauncher = Join-Path $MemgwOps 'memgw-silent-launch.vbs'

function New-MemgwHiddenScriptAction {
    <#
    .SYNOPSIS
        Windowless action for one of this toolkit's own .ps1 files.
    .DESCRIPTION
        The memgw installers all run a PowerShell script under the same fixed
        flags, so the executable and its argv are named once here. A task that is
        NOT ours -- rag-backup-pull runs pwsh.exe -- must use
        New-MemgwHiddenAction with its own original argv instead.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$ScriptPath,
        [string[]]$ExtraArgs = @(),
        [AllowEmptyString()][string]$WorkingDirectory = $script:MemgwRoot
    )
    $argv = @(
        '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass'
        '-WindowStyle', 'Hidden', '-File', $ScriptPath
    ) + $ExtraArgs
    return New-MemgwHiddenAction -Executable (Get-Command powershell.exe).Source `
        -Arguments $argv -WorkingDirectory $WorkingDirectory
}

function New-MemgwHiddenAction {
    <#
    .SYNOPSIS
        Wrap an executable and its argv in a windowless Scheduled Task action.
    .DESCRIPTION
        A task action that is a console-subsystem image (powershell.exe, pwsh.exe,
        python.exe) gets a console from Windows; -WindowStyle Hidden only hides
        the host window, so the console still appears as a terminal window on
        every run. Routing through memgw-silent-launch.vbs avoids creating one,
        and the child's exit code still reaches "Last Run Result".

        The executable and its arguments are wrapped VERBATIM. Nothing about the
        program's own options is chosen here, because converting an existing task
        must not change its semantics: pwsh.exe and powershell.exe are different
        programs with different native-command stdout handling, and substituting
        one for the other is a real behaviour change -- PS 5.1's UTF-16LE
        redirect is what corrupted a pg_dump in this fleet before.
    .PARAMETER Executable
        The program to run.
    .PARAMETER Arguments
        Its arguments, exactly as the task should pass them.
    .PARAMETER WorkingDirectory
        Working directory for the action. Empty means none, which is a real state
        an existing task may be in; the argument is then omitted.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Executable,
        [string[]]$Arguments = @(),
        [AllowEmptyString()][string]$WorkingDirectory = $script:MemgwRoot
    )

    if (-not (Test-Path -LiteralPath $Executable)) {
        throw "executable not found: $Executable"
    }
    if (-not (Test-Path -LiteralPath $script:MemgwSilentLauncher)) {
        throw "silent launcher not found: $($script:MemgwSilentLauncher)"
    }

    # A value carrying a double quote is refused rather than re-quoted: the
    # launcher is a Windows Script Host script whose own command-line splitter
    # does not honour backslash-escaped quotes, so such a value would arrive
    # corrupted. Refusing here keeps the helper honest about that limit instead
    # of installing an action that silently passes a wrong argument.
    foreach ($x in @($Executable) + $Arguments) {
        if ($x -match '"') {
            throw "a double quote is not supported in a task argument value: $x"
        }
    }

    $argument = @(
        '//B'
        '//Nologo'
        ConvertTo-MemgwArg -Value $script:MemgwSilentLauncher
        ConvertTo-MemgwArg -Value $Executable
    ) + ($Arguments | ForEach-Object { ConvertTo-MemgwArg -Value $_ })

    # New-ScheduledTaskAction rejects an empty -WorkingDirectory, and "no working
    # directory" is a real state an existing task may be in.
    $params = @{
        Execute  = Join-Path $env:SystemRoot 'System32\wscript.exe'
        Argument = ($argument -join ' ')
    }
    if ($WorkingDirectory) { $params.WorkingDirectory = $WorkingDirectory }
    return New-ScheduledTaskAction @params
}

function Get-MemgwTaskHash {
    <#
    .SYNOPSIS
        A stable hash of a task's action, for the install manifest.
    .DESCRIPTION
        The uninstaller compares this against what it recorded. If an operator
        has edited the task by hand, the hash no longer matches and the
        uninstaller refuses rather than deleting something it did not create.

        The hash covers only the action (what runs), not volatile state such as
        LastRunTime, so a task that has simply run is still recognised.
    #>
    [CmdletBinding()]
    param([Parameter(Mandatory)]$Task)

    $parts = foreach ($a in $Task.Actions) {
        "$($a.Execute)|$($a.Arguments)|$($a.WorkingDirectory)"
    }
    $joined = ($parts -join "`n")
    $bytes  = [System.Text.Encoding]::UTF8.GetBytes($joined)
    $sha    = [System.Security.Cryptography.SHA256]::Create()
    try {
        return ([System.BitConverter]::ToString($sha.ComputeHash($bytes)) -replace '-', '').ToLowerInvariant()
    } finally {
        $sha.Dispose()
    }
}
