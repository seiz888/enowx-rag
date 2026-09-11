<#
.SYNOPSIS
    Copy the sealed memgw ledger backup to a different failure domain (the VPS)
    and verify it there.

.DESCRIPTION
    The ledger backup lives at D:\memgw-backups on the same machine as the
    ledger itself. That is a different physical volume, but it is not a
    different failure domain: a lost or stolen workstation takes both. This
    script is the off-host half — it pushes the newest sealed ledger artefact to
    the VPS and verifies the copy it lands on.

    What travels, and what cannot
    -----------------------------
    Only the DPAPI-sealed artefact, its manifest and its sha256 travel. The
    seal is bound to this Windows account's DPAPI master key, so the VPS holds
    a copy it cannot read and no key ever leaves the machine. That is the point:
    the off-host copy protects against losing the workstation, and the DPAPI
    binding protects against losing the VPS.

    Verification, not hope
    ----------------------
    A copy nobody has checked is a hypothesis. After the transfer the script
    asks the VPS for the size and sha256 of the file it received and compares
    both to the local artefact; it also confirms the remote permissions are
    root-only. A mismatch is a failure, and the script exits non-zero so the
    Scheduled Task records it.

    Restore round-trip
    ------------------
    -VerifyRestore additionally pulls the remote copy back down to a temporary
    path, unseals it, restores it into a disposable database, and compares the
    event/receipt/sequence counts against the live ledger. That is the
    difference between "the bytes arrived" and "the backup is recoverable".

    Retention
    ---------
    Keeps the newest $Keep artefacts remotely, pruned by the remote copy rather
    than the local one, so a machine that cannot reach the VPS for a while does
    not silently delete its own history when it reconnects.

.PARAMETER Keep
    Remote sealed artefacts to retain (default 7).

.PARAMETER VerifyRestore
    Also pull the remote copy back, unseal it and restore into a disposable
    database, comparing counts with the live ledger.

.PARAMETER WhatIf
    Report what would be transferred without transferring.

.NOTES
    Read-only with respect to the ledger itself. Touches no protected resource.
    No secret value is printed: the artefact is already sealed, and its sha256
    is a digest, not a credential.
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [int]$Keep = 7,
    [switch]$VerifyRestore,
    [string]$Dest = 'D:\memgw-backups'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$VpsHost = '168.110.218.207'
$VpsUser = 'ubuntu'
$SshKey  = "$env:USERPROFILE\.ssh\id_oracle"
$RemoteDir = '/opt/rag-backup/memgw-ledger-offhost'
$LogFile = Join-Path $Dest 'ledger-offhost.log'

function Write-Log {
    param([string]$Message)
    $line = "[$(Get-Date -Format 'yyyy-MM-ddTHH:mm:sszzz')] $Message"
    Write-Host $line
    Add-Content -LiteralPath $LogFile -Value $line -Encoding utf8
}

function Get-Sha256Hex {
    param([string]$Path)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $stream = [System.IO.File]::OpenRead($Path)
        try { return ($sha.ComputeHash($stream) | ForEach-Object { $_.ToString('x2') }) -join '' }
        finally { $stream.Dispose() }
    } finally { $sha.Dispose() }
}

function Invoke-Docker {
    <#
    .SYNOPSIS
        Run docker, returning its output without letting stderr throw.
    .DESCRIPTION
        Under $ErrorActionPreference='Stop', Windows PowerShell 5.1 turns a
        native command's stderr into a terminating error even when stderr is
        redirected. psql writes "NOTICE: database ... does not exist, skipping"
        to stderr for a perfectly successful DROP ... IF EXISTS, so that
        preference has to be relaxed for these calls only.
    #>
    param([Parameter(Mandatory)][string[]]$ArgumentList)
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        return (& docker @ArgumentList 2>&1 | Where-Object { $_ -notmatch '^NOTICE:' })
    } finally {
        $ErrorActionPreference = $previous
    }
}

$sshArgs = @('-i', $SshKey, '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15')
$sshTarget = "$VpsUser@$VpsHost"

# --- pick the newest sealed ledger artefact ---------------------------------
$newest = Get-ChildItem -LiteralPath $Dest -Filter 'memgw-*.dump.dpapi' -ErrorAction SilentlyContinue |
    Sort-Object LastWriteTime -Descending | Select-Object -First 1
if ($null -eq $newest) { throw "no sealed ledger backup in $Dest" }

$manifestPath = "$($newest.FullName).manifest"
if (-not (Test-Path -LiteralPath $manifestPath)) { throw "no manifest beside $($newest.Name)" }

# Refuse to ship the artefact the audit found: a text-mangled dump restores
# nothing, and putting a useless file in a second place makes it harder to
# notice, not easier.
$toc = $null
foreach ($line in (Get-Content -LiteralPath $manifestPath)) {
    if ($line -match '^toc_entries=(\d+)') { $toc = [int]$Matches[1] }
}
if ($null -eq $toc -or $toc -lt 100) {
    throw "$($newest.Name) records $toc TOC entries; refusing to ship a backup that is not one"
}

$localSha = Get-Sha256Hex -Path $newest.FullName
$localSize = $newest.Length
Write-Log "artefact: $($newest.Name) ($localSize bytes, $toc TOC entries, sha256 $localSha)"

if ($WhatIfPreference) {
    Write-Host "would transfer $($newest.Name) (+manifest +sha256) to ${sshTarget}:$RemoteDir"
    exit 0
}

# --- ensure the remote directory exists, root-only --------------------------
# Upload goes to a world-writable temp path; `install` then places it with
# root-only ownership, so the final location is never briefly readable by the
# unprivileged ssh account.
$mkdir = & ssh @sshArgs $sshTarget "sudo mkdir -p $RemoteDir && sudo chown root:root $RemoteDir && sudo chmod 0700 $RemoteDir && echo ok" 2>&1
if (("$mkdir" -join ' ') -notmatch 'ok') { throw "could not prepare ${RemoteDir}: $($mkdir -join ' ')" }

# Remote scripts go in over stdin (`ssh host bash -s`), never as an argument.
# Windows ships its own sudo.exe, and PowerShell hands a multi-line argument to
# ssh in a way the remote shell does not see whole -- the local sudo ran
# instead, and its "Sudo is disabled on this machine" went into stdout. A pipe
# has no quoting to get wrong. The script itself carries `sudo` per line
# (passwordless for this account), because a here-doc inside a script that is
# itself arriving on stdin has nothing left to read.
function Invoke-RemoteScript {
    param([Parameter(Mandatory)][string]$Script)
    $out = $Script | & ssh @sshArgs $sshTarget 'sudo bash -s' 2>&1
    if ($LASTEXITCODE -ne 0) { throw "remote script failed (exit $LASTEXITCODE): $(($out | Select-Object -Last 3) -join ' | ')" }
    return $out
}

# --- transfer ---------------------------------------------------------------
$sumFile = Join-Path $env:TEMP "$($newest.Name).sha256"
Set-Content -LiteralPath $sumFile -Value "$localSha  $($newest.Name)" -Encoding ascii
$batch = @(
    "put -p `"$($newest.FullName)`" /tmp/$($newest.Name).part"
    "put -p `"$manifestPath`" /tmp/$($newest.Name).manifest"
    "put -p `"$sumFile`" /tmp/$($newest.Name).sha256"
) -join "`n"
$batchFile = Join-Path $env:TEMP "memgw-push-$([guid]::NewGuid().ToString('N')).txt"
Set-Content -LiteralPath $batchFile -Value $batch -Encoding ascii
try {
    $out = & sftp -i $SshKey -o BatchMode=yes -b $batchFile $sshTarget 2>&1
    if ($LASTEXITCODE -ne 0) { throw "sftp failed (exit $LASTEXITCODE): $(($out | Select-Object -Last 3) -join ' | ')" }
} finally {
    Remove-Item -LiteralPath $batchFile -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $sumFile -Force -ErrorAction SilentlyContinue
}

# --- place with root-only permissions, and verify what landed ---------------
$placeScript = @"
set -e
install -o root -g root -m 0600 /tmp/$($newest.Name).part $RemoteDir/$($newest.Name)
install -o root -g root -m 0600 /tmp/$($newest.Name).manifest $RemoteDir/$($newest.Name).manifest
install -o root -g root -m 0600 /tmp/$($newest.Name).sha256 $RemoteDir/$($newest.Name).sha256
rm -f /tmp/$($newest.Name).part /tmp/$($newest.Name).manifest /tmp/$($newest.Name).sha256
echo "size=`$(stat -c %s $RemoteDir/$($newest.Name))"
echo "sha=`$(sha256sum $RemoteDir/$($newest.Name) | cut -d' ' -f1)"
echo "mode=`$(stat -c %a $RemoteDir/$($newest.Name))"
echo "owner=`$(stat -c %U:%G $RemoteDir/$($newest.Name))"
"@
$place = Invoke-RemoteScript -Script $placeScript

$remoteSize = ($place | Where-Object { $_ -match '^size=' }) -replace '^size=', ''
$remoteSha = ($place | Where-Object { $_ -match '^sha=' }) -replace '^sha=', ''
$remoteMode = ($place | Where-Object { $_ -match '^mode=' }) -replace '^mode=', ''
$remoteOwner = ($place | Where-Object { $_ -match '^owner=' }) -replace '^owner=', ''

if ("$remoteSize".Trim() -ne "$localSize") {
    throw "size mismatch: local $localSize, remote $("$remoteSize".Trim())"
}
if ("$remoteSha".Trim().ToLowerInvariant() -ne $localSha.ToLowerInvariant()) {
    throw "sha256 mismatch: local $localSha, remote $("$remoteSha".Trim())"
}
if ("$remoteMode".Trim() -ne '600' -or "$remoteOwner".Trim() -ne 'root:root') {
    throw "remote permissions are $remoteOwner $remoteMode; expected root:root 600"
}
Write-Log "verified remote: $($newest.Name) size=$("$remoteSize".Trim()) sha256=$("$remoteSha".Trim()) mode=$("$remoteMode".Trim())"

# --- remote retention -------------------------------------------------------
$keepScript = @"
set -e
cd $RemoteDir
ls -1t memgw-*.dump.dpapi 2>/dev/null | tail -n +$($Keep + 1) | while read -r f; do
  rm -f "`$f" "`$f.manifest" "`$f.sha256"
  echo "pruned `$f"
done
echo "kept=`$(ls -1 memgw-*.dump.dpapi 2>/dev/null | wc -l)"
"@
$pruned = Invoke-RemoteScript -Script $keepScript
Write-Log "remote retention: $(($pruned | Where-Object { $_ -match '^(pruned|kept)' }) -join '; ')"

# --- restore round-trip from the REMOTE copy --------------------------------
if ($VerifyRestore) {
    # Pull the remote copy back into a separate file so the restore provably
    # uses what the VPS holds, not the local original.
    $back = Join-Path $env:TEMP "offhost-$($newest.Name)"
    Remove-Item -LiteralPath $back -Force -ErrorAction SilentlyContinue

    # The ssh account cannot read a 0600 root file, so borrow it with a
    # root-owned temp copy the same way a recovery operator would.
    $stage = "/tmp/$($newest.Name).verify"
    $prep = & ssh @sshArgs $sshTarget "sudo install -o $VpsUser -g $VpsUser -m 0600 $RemoteDir/$($newest.Name) $stage && sudo chown ${VpsUser}:${VpsUser} $stage && echo ok" 2>&1
    if (("$prep" -join ' ') -notmatch 'ok') { throw "could not stage the remote copy for verification: $($prep -join ' ')" }

    $getBatch = "get -p $stage `"$back`""
    $getFile = Join-Path $env:TEMP "memgw-getback-$([guid]::NewGuid().ToString('N')).txt"
    Set-Content -LiteralPath $getFile -Value $getBatch -Encoding ascii
    try {
        $null = & sftp -i $SshKey -o BatchMode=yes -b $getFile $sshTarget 2>&1
        if ($LASTEXITCODE -ne 0) { throw "could not pull the remote copy back (exit $LASTEXITCODE)" }
    } finally {
        Remove-Item -LiteralPath $getFile -Force -ErrorAction SilentlyContinue
        $null = & ssh @sshArgs $sshTarget "rm -f $stage" 2>&1
    }

    $backSha = Get-Sha256Hex -Path $back
    if ($backSha -ne $localSha) { throw "the copy pulled back from the VPS is not the artefact that was sent" }

    $target = 'memgw_offhostverify'
    $null = Invoke-Docker -ArgumentList @('exec', '-u', 'postgres', 'memgw-live', 'psql', '-U', 'postgres', '-c', "DROP DATABASE IF EXISTS $target")
    # restore.ps1 lives with the ledger tooling at D:\memgw, not in this repo's
    # ops directory; fall back to the repo copy so the script works either way.
    $restoreScript = Join-Path 'D:\memgw' 'restore.ps1'
    if (-not (Test-Path -LiteralPath $restoreScript)) {
        $restoreScript = Join-Path $PSScriptRoot 'restore.ps1'
    }
    if (-not (Test-Path -LiteralPath $restoreScript)) { throw "restore.ps1 not found (looked in D:\memgw and $PSScriptRoot)" }
    $rout = & powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $restoreScript -Sealed $back -Target $target 2>&1
    if ($LASTEXITCODE -ne 0) { throw "restore from the remote copy failed: $(($rout | Select-Object -Last 3) -join ' | ')" }

    $q = "SELECT 'events='||(SELECT count(*) FROM memgw.events)||' receipts='||(SELECT count(*) FROM memgw.write_receipts)||' max_seq='||(SELECT COALESCE(max(seq),0) FROM memgw.events)"
    $liveCounts = ((Invoke-Docker -ArgumentList @('exec', '-u', 'postgres', 'memgw-live', 'psql', '-U', 'postgres', '-d', 'memgw', '-tAc', $q)) -join '').Trim()
    $backCounts = ((Invoke-Docker -ArgumentList @('exec', '-u', 'postgres', 'memgw-live', 'psql', '-U', 'postgres', '-d', $target, '-tAc', $q)) -join '').Trim()
    Remove-Item -LiteralPath $back -Force -ErrorAction SilentlyContinue

    if ($liveCounts -ne $backCounts) {
        throw "restore round-trip mismatch: live '$liveCounts' vs restored '$backCounts'"
    }
    Write-Log "restore round-trip OK from the remote copy: $liveCounts"
    $null = Invoke-Docker -ArgumentList @('exec', '-u', 'postgres', 'memgw-live', 'psql', '-U', 'postgres', '-c', "DROP DATABASE IF EXISTS $target")
}

Write-Log 'done: off-host ledger copy verified'
exit 0
