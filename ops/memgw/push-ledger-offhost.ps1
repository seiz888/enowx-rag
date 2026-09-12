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
    The envelope artefact travels: `<stamp>.aes` (the payload), plus
    `<stamp>.key.recovery` and `<stamp>.key.recovery.fingerprint`, its manifest
    and its sha256. The data key is wrapped to the recovery public key whose
    private half is escrowed in the Multi-Brain vault, so the VPS holds bytes it
    cannot read while the owner can still open them after losing this machine.

    v2 format. The payload is an authenticated envelope (AES-256-GCM in chained
    segments, `MEMGWSE2`), so there is no separate `.iv` file -- the nonce prefix
    is inside the authenticated header. An artefact that still carries a `.iv`
    sidecar is the older v1 (AES-CBC) format, which cannot detect tampering; it
    is still readable for archival recovery but is no longer produced.

    The DPAPI copy (`<stamp>.key.dpapi`) is deliberately NOT sent. It is the
    local-convenience wrapper, and shipping it would place a copy on the VPS
    that only this Windows profile can open — the exact defect the envelope
    scheme exists to remove. An earlier revision of this script shipped the
    DPAPI-sealed payload and nothing else, which made the off-host copy
    unreadable by anyone after the workstation was lost.

    A backup taken before the envelope scheme (`*.dump.dpapi` payloads) has no
    recovery wrapper and cannot be shipped here; this script fails rather than
    offer a copy nobody could restore.

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
    [string]$Dest = 'D:\memgw-backups',
    # The ledger container. Only the -VerifyRestore reconciliation reads it;
    # nothing here writes to the ledger itself.
    [string]$ContainerName = 'memgw-live'
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

# --- pick the newest envelope ledger artefact --------------------------------
# `.aes` is the envelope payload; `.key.dpapi` is only its local key wrapper and
# is never a candidate, or the selection would match a file that is not a dump.
# The prefix is anchored so `latest.manifest` and the sidecars cannot be picked.
$newest = Get-ChildItem -LiteralPath $Dest -Filter 'memgw-*.dump.aes' -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -match '^memgw-\d{8}-\d{6}\.dump\.aes$' } |
    Sort-Object LastWriteTime -Descending | Select-Object -First 1
if ($null -eq $newest) {
    # Say what is actually there. "No backup" and "a backup this script cannot
    # ship" are different problems and must not read as the same one.
    $legacy = @(Get-ChildItem -LiteralPath $Dest -Filter 'memgw-*.dump.dpapi' -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -match '^memgw-\d{8}-\d{6}\.dump\.dpapi$' })
    if ($legacy.Count -gt 0) {
        throw "no envelope ledger backup in $Dest; $($legacy.Count) pre-envelope artifact(s) present (e.g. $($legacy[0].Name)), which carry no recovery wrapper and cannot be shipped off-host"
    }
    throw "no sealed ledger backup in $Dest"
}

# The stem is `<stamp>.dump`; every part of the artefact must be present or the
# copy on the VPS is not recoverable. A missing `.key.recovery` means only the
# DPAPI path exists, which is the defect this whole file exists to prevent.
$stem = $newest.FullName -replace '\.aes$', ''
$manifestPath = "$stem.manifest"
# v2 carries its nonce inside the authenticated header, so there is no `.iv`
# sidecar to require. A `.iv` present indicates a v1 artefact; that is refused
# explicitly rather than shipped, because v1 cannot detect tampering and the
# whole point of the off-host copy is that it can be trusted months later.
if (Test-Path -LiteralPath "$stem.iv") {
    throw "$($newest.Name) is a v1 (AES-CBC) artefact: it has an .iv sidecar and no authenticated format. It is readable for archival recovery but must not be shipped as a new off-host backup."
}
$parts = @{
    keyRecovery    = "$stem.key.recovery"
    fingerprint    = "$stem.key.recovery.fingerprint"
}
foreach ($p in @($manifestPath) + $parts.Values) {
    if (-not (Test-Path -LiteralPath $p)) { throw "envelope artefact is incomplete: missing $(Split-Path -Leaf $p)" }
}

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

# The manifest and the fingerprint file must agree about which key opens this.
# If they disagree the artefact is not the one its own manifest describes, and
# shipping it would put a copy on the VPS that the escrowed key cannot open.
$fingerprint = (Get-Content -LiteralPath $parts.fingerprint -Raw).Trim()
$manifestFingerprint = $null
foreach ($line in (Get-Content -LiteralPath $manifestPath)) {
    if ($line -match '^recovery_fingerprint=(\S+)') { $manifestFingerprint = $Matches[1] }
}
if (-not $manifestFingerprint) { throw "$($newest.Name) manifest records no recovery_fingerprint" }
if ($fingerprint -ne $manifestFingerprint) {
    throw "recovery fingerprint mismatch: sidecar $fingerprint, manifest $manifestFingerprint"
}

$localSha = Get-Sha256Hex -Path $newest.FullName
$localSize = $newest.Length
Write-Log "artefact: $($newest.Name) ($localSize bytes, $toc TOC entries, sha256 $localSha, recovery $fingerprint)"

if ($WhatIfPreference) {
    Write-Host "would transfer $($newest.Name) (+manifest +sha256 +key.recovery +fingerprint) to ${sshTarget}:$RemoteDir"
    exit 0
}

# Which files make up the artefact, and therefore what travels and what is
# removed together. The naming rule is stated once here so the transfer, the
# placement and the remote retention cannot drift apart again.
$artefactFiles = @($newest.Name, "$($newest.Name).manifest", "$($newest.Name).sha256",
                   "$($newest.Name).key.recovery",
                   "$($newest.Name).key.recovery.fingerprint")

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
# Every part travels. The `.aes` payload alone is useless: `.key.recovery`
# is what turns it back into a dump, and the fingerprint is how a recovery
# operator learns which key to fetch without opening anything.
$batch = @(
    "put -p `"$($newest.FullName)`" /tmp/$($newest.Name).part"
    "put -p `"$manifestPath`" /tmp/$($newest.Name).manifest"
    "put -p `"$sumFile`" /tmp/$($newest.Name).sha256"
    "put -p `"$($parts.keyRecovery)`" /tmp/$($newest.Name).key.recovery"
    "put -p `"$($parts.fingerprint)`" /tmp/$($newest.Name).key.recovery.fingerprint"
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
install -o root -g root -m 0600 /tmp/$($newest.Name).key.recovery $RemoteDir/$($newest.Name).key.recovery
install -o root -g root -m 0600 /tmp/$($newest.Name).key.recovery.fingerprint $RemoteDir/$($newest.Name).key.recovery.fingerprint
rm -f /tmp/$($newest.Name).part /tmp/$($newest.Name).manifest /tmp/$($newest.Name).sha256 \
      /tmp/$($newest.Name).key.recovery /tmp/$($newest.Name).key.recovery.fingerprint
echo "size=`$(stat -c %s $RemoteDir/$($newest.Name))"
echo "sha=`$(sha256sum $RemoteDir/$($newest.Name) | cut -d' ' -f1)"
echo "mode=`$(stat -c %a $RemoteDir/$($newest.Name))"
echo "owner=`$(stat -c %U:%G $RemoteDir/$($newest.Name))"
echo "parts=`$(ls -1 $RemoteDir/$($newest.Name).key.recovery $RemoteDir/$($newest.Name).key.recovery.fingerprint 2>/dev/null | wc -l)"
"@
$place = Invoke-RemoteScript -Script $placeScript

$remoteSize = ($place | Where-Object { $_ -match '^size=' }) -replace '^size=', ''
$remoteSha = ($place | Where-Object { $_ -match '^sha=' }) -replace '^sha=', ''
$remoteMode = ($place | Where-Object { $_ -match '^mode=' }) -replace '^mode=', ''
$remoteOwner = ($place | Where-Object { $_ -match '^owner=' }) -replace '^owner=', ''
$remoteParts = ($place | Where-Object { $_ -match '^parts=' }) -replace '^parts=', ''

if ("$remoteSize".Trim() -ne "$localSize") {
    throw "size mismatch: local $localSize, remote $("$remoteSize".Trim())"
}
if ("$remoteSha".Trim().ToLowerInvariant() -ne $localSha.ToLowerInvariant()) {
    throw "sha256 mismatch: local $localSha, remote $("$remoteSha".Trim())"
}
if ("$remoteMode".Trim() -ne '600' -or "$remoteOwner".Trim() -ne 'root:root') {
    throw "remote permissions are $remoteOwner $remoteMode; expected root:root 600"
}
# A payload whose key wrapper did not land is a copy nobody can open. Check the
# count, not the intent.
if ("$remoteParts".Trim() -ne '2') {
    throw "only $("$remoteParts".Trim())/2 envelope parts landed on the VPS; the copy is not recoverable"
}
Write-Log "verified remote: $($newest.Name) size=$("$remoteSize".Trim()) sha256=$("$remoteSha".Trim()) mode=$("$remoteMode".Trim()) parts=2/2 recovery=$fingerprint"

# --- remote retention -------------------------------------------------------
$keepScript = @"
set -e
cd $RemoteDir
# Prune whole artefacts, not payloads. A kept `.aes` whose key wrapper was
# pruned is unopenable, so every part of a pruned artefact goes together -- and
# every part of a kept one stays.
ls -1t memgw-*.dump.aes 2>/dev/null | tail -n +$($Keep + 1) | while read -r f; do
  rm -f "`$f" "`$f.manifest" "`$f.sha256" "`$f.key.recovery" "`$f.key.recovery.fingerprint"
  echo "pruned `$f"
done
# Report the count of complete artefacts, so a half-pruned one is visible.
kept=0
for f in memgw-*.dump.aes; do
  [ -e "`$f" ] || continue
  if [ -e "`$f.key.recovery" ] && [ -e "`$f.manifest" ]; then kept=`$((kept+1)); fi
done
echo "kept=`$kept"
"@
$pruned = Invoke-RemoteScript -Script $keepScript
Write-Log "remote retention: $(($pruned | Where-Object { $_ -match '^(pruned|kept)' }) -join '; ')"

# --- recovery round-trip from the REMOTE copy, using the ESCROWED key -------
#
# A restore that unseals with DPAPI proves only that this machine can read its
# own copy. The claim this whole file exists to support is the opposite one: the
# copy on the VPS must be openable by someone who has lost this machine. So the
# verification path fetches the private recovery key from the vault, unwraps the
# data key with it, and restores from that -- DPAPI is not involved anywhere.
if ($VerifyRestore) {
    $work = Join-Path $env:TEMP ("offhost-verify-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $work | Out-Null
    $target = 'memgw_offhostverify'

    try {
        # 1. Bring down the artefact's parts, not just the payload. Staging them
        #    as the ssh user needs a root-owned copy first, exactly as a recovery
        #    operator would borrow it.
        $stageDir = "/tmp/$($newest.Name).verify"
        $prep = & ssh @sshArgs $sshTarget "sudo install -d -o $VpsUser -g $VpsUser -m 0700 $stageDir && for f in $($newest.Name) $($newest.Name).key.recovery $($newest.Name).key.recovery.fingerprint $($newest.Name).manifest; do sudo install -o $VpsUser -g $VpsUser -m 0600 $RemoteDir/`$f $stageDir/`$f; done && echo ok" 2>&1
        if (("$prep" -join ' ') -notmatch 'ok') { throw "could not stage the remote artefact: $($prep -join ' ')" }

        $getBatch = @(
            "get -p $stageDir/$($newest.Name) `"$work\$($newest.Name)`""
            "get -p $stageDir/$($newest.Name).key.recovery `"$work\$($newest.Name).key.recovery`""
            "get -p $stageDir/$($newest.Name).manifest `"$work\$($newest.Name).manifest`""
        ) -join "`n"
        $getFile = Join-Path $env:TEMP "memgw-getback-$([guid]::NewGuid().ToString('N')).txt"
        Set-Content -LiteralPath $getFile -Value $getBatch -Encoding ascii
        try {
            $null = & sftp -i $SshKey -o BatchMode=yes -b $getFile $sshTarget 2>&1
            if ($LASTEXITCODE -ne 0) { throw "could not pull the remote artefact back (exit $LASTEXITCODE)" }
        } finally {
            Remove-Item -LiteralPath $getFile -Force -ErrorAction SilentlyContinue
            $null = & ssh @sshArgs $sshTarget "rm -rf $stageDir" 2>&1
        }

        $back = Join-Path $work $newest.Name
        if ((Get-Sha256Hex -Path $back) -ne $localSha) {
            throw "the payload pulled back from the VPS is not the artefact that was sent"
        }

        # 2. Fetch the escrowed private key. It is written straight to disk and
        #    never echoed; the vault is the only place it exists.
        $envelope = Join-Path 'D:\PROJECTS\enowx-rag\ops\memgw' 'ledger-envelope.py'
        if (-not (Test-Path -LiteralPath $envelope)) { $envelope = Join-Path $PSScriptRoot 'ledger-envelope.py' }
        if (-not (Test-Path -LiteralPath $envelope)) { throw 'ledger-envelope.py not found' }
        $vaultGet = Join-Path 'D:\PROJECTS\enowx-rag\ops\memgw' 'vault-get.py'
        if (-not (Test-Path -LiteralPath $vaultGet)) { $vaultGet = Join-Path $PSScriptRoot 'vault-get.py' }
        if (-not (Test-Path -LiteralPath $vaultGet)) { throw 'vault-get.py not found' }

        $priv = Join-Path $work 'recovery.pem'
        & python $vaultGet 'keys/memgw-ledger-recovery-20260911.pem' $priv 2>&1 | Out-Null
        if (-not (Test-Path -LiteralPath $priv)) { throw 'could not fetch the escrowed recovery key from the vault' }

        $dataKey = Join-Path $work 'data.key'
        $u = & python $envelope unwrap-recovery --key-recovery "$back.key.recovery" --recovery-key $priv --out $dataKey 2>&1
        if ($LASTEXITCODE -ne 0) { throw "the escrowed key did not unwrap the data key: $(($u | Select-Object -Last 2) -join ' ')" }

        $plain = Join-Path $work 'ledger.dump'
        $d = & python $envelope unseal --input $back --key-file $dataKey --out $plain 2>&1
        if ($LASTEXITCODE -ne 0) { throw "unseal with the recovered data key failed: $(($d | Select-Object -Last 2) -join ' ')" }

        # 3. The unsealed bytes must be a pg dump, not merely bytes.
        $bytes = [System.IO.File]::ReadAllBytes($plain)
        $magic = (($bytes[0..4]) | ForEach-Object { $_.ToString('x2') }) -join ''
        if ($magic -ne '5047444d50') { throw "recovered payload is not a pg custom-format archive (magic $magic)" }
        Remove-Item -LiteralPath $priv, $dataKey -Force -ErrorAction SilentlyContinue

        # 4. Restore into a disposable database and reconcile.
        $null = Invoke-Docker -ArgumentList @('exec', '-u', 'postgres', $ContainerName, 'psql', '-U', 'postgres', '-c', "DROP DATABASE IF EXISTS $target")
        $null = Invoke-Docker -ArgumentList @('exec', '-u', 'postgres', $ContainerName, 'psql', '-U', 'postgres', '-c', "CREATE DATABASE $target OWNER memgw_migrator")
        $inContainer = "/tmp/$($newest.Name).restore"
        $null = Invoke-Docker -ArgumentList @('cp', $plain, "${ContainerName}:${inContainer}")
        $r = Invoke-Docker -ArgumentList @('exec', '-u', 'postgres', $ContainerName, 'pg_restore', '-U', 'postgres', '--exit-on-error', '-d', $target, $inContainer)
        if ($LASTEXITCODE -ne 0) { throw "pg_restore from the recovered dump failed: $(($r | Select-Object -Last 2) -join ' ')" }
        $null = Invoke-Docker -ArgumentList @('exec', $ContainerName, 'rm', '-f', $inContainer)

        # Reconcile against the MANIFEST, not against the live ledger.
        #
        # The ledger is live: it commits events continuously, so comparing a
        # frozen dump to the current database fails whenever any event lands
        # between the dump and this read. That is a race in the check, not a
        # fault in the backup -- observed directly: a dump taken at 133 events
        # was compared against 134 because one more event committed in between.
        # A verification that fails for reasons unrelated to the artefact is a
        # verification nobody will trust.
        #
        # The manifest is the right reference because it is the backup's own
        # provenance: it was written from the database at dump time, and it
        # travels with the artefact. Reconciling against it proves that what
        # came back out of the envelope is what went in.
        $q = "SELECT 'events='||(SELECT count(*) FROM memgw.events)||' receipts='||(SELECT count(*) FROM memgw.write_receipts)||' max_seq='||(SELECT COALESCE(max(seq),0) FROM memgw.events)"
        $backCounts = ((Invoke-Docker -ArgumentList @('exec', '-u', 'postgres', $ContainerName, 'psql', '-U', 'postgres', '-d', $target, '-tAc', $q)) -join '').Trim()

        $manifestCounts = $null
        foreach ($line in (Get-Content -LiteralPath "$back.manifest" -ErrorAction SilentlyContinue)) {
            if ($line -match '^counts=(.+)$') { $manifestCounts = $Matches[1].Trim() }
        }
        if (-not $manifestCounts) { throw "the pulled artefact's manifest records no counts; there is nothing to reconcile against" }

        if ($backCounts -ne $manifestCounts) {
            throw "recovery reconciliation failed: the manifest records '$manifestCounts' but the dump recovered through the escrowed key contains '$backCounts'"
        }
        Write-Log "recovery round-trip OK (escrowed key, no DPAPI): $backCounts == manifest"
        $null = Invoke-Docker -ArgumentList @('exec', '-u', 'postgres', $ContainerName, 'psql', '-U', 'postgres', '-c', "DROP DATABASE IF EXISTS $target")
    }
    finally {
        Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Write-Log 'done: off-host ledger copy verified'
exit 0
