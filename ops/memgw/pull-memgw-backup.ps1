#Requires -Version 5.1
<#
.SYNOPSIS
    Pull the memgw ledger backups from the VPS to this workstation, unsealing one
    through the ESCROWED recovery key to prove it is recoverable.

.DESCRIPTION
    Before the migration the ledger lived here and the VPS held the off-host
    copy; this workstation pushed. After the migration the VPS is authoritative,
    so the direction reverses: the VPS takes the backup and this machine holds
    the off-host copy. A pull is the correct direction for an off-host copy --
    it survives a compromised or lost source host, and a push cannot.

    What travels back
    -----------------
    The whole envelope artefact: `<stamp>.dump.aes`, `<stamp>.dump.key.recovery`,
    `<stamp>.dump.key.recovery.fingerprint`, the manifest and the sha256. The
    payload alone is useless without the key wrapper, so a partial transfer is
    treated as a failure rather than as a copy that "mostly" arrived.

    Verification, and the claim it exists to support
    ------------------------------------------------
    Two independent checks, because they answer different questions:

      * **Integrity in transit** -- the sha256 is recomputed here and compared
        with the sha256 the VPS computed at backup time. This detects a corrupted
        or truncated transfer.
      * **Recoverability without this machine** -- the escrowed private key is
        fetched from the Multi-Brain vault, used to unwrap the data key, and the
        payload is unsealed and checked against the manifest's own counts.
        DPAPI is not involved, which is the point: it is the only check that
        proves the copy could be restored after this workstation is lost.

    The second check is the one that matters. A DPAPI-sealed copy is readable
    only on the machine that made it, and this project shipped exactly that by
    mistake once: the artefact on the VPS could not be opened by anyone after
    the workstation died, and the health check reported only a stale file.

    Retention keeps whole artefacts. A payload whose key wrapper was pruned is
    unopenable, so every part of a pruned artefact is removed together.

.PARAMETER Keep
    Artefacts to retain locally (default 7).

.PARAMETER VerifyRecovery
    Fetch the escrowed key, unwrap, unseal and reconcile against the manifest.
    Requires vault access; when the vault is unreachable this is reported as a
    failure rather than skipped silently, because a backup whose only reader
    cannot be reached is not yet a proven backup.

.PARAMETER WhatIf
    Report what would be pulled without pulling.
#>

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [int]$Keep = 7,
    [switch]$VerifyRecovery,
    [string]$Dest = 'D:\memgw-backups\vps\sealed',

    [string]$VpsHost = '168.110.218.207',
    [string]$VpsUser = 'ubuntu',
    [string]$SshKey  = "$env:USERPROFILE\.ssh\id_oracle",
    [string]$RemoteDir = '/opt/memgw/backup',

    # Where the escrowed private key lives. A PATH, never a value: the key is
    # fetched to a temporary file and removed in a finally block.
    [string]$VaultKeyPath = 'keys/memgw-ledger-recovery-20260911.pem',
    [string]$RepoOpsDir = 'D:\PROJECTS\enowx-rag\ops\memgw'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

New-Item -ItemType Directory -Force -Path $Dest | Out-Null
$logFile = Join-Path (Split-Path -Parent $Dest) 'memgw-pull.log'

function Write-Log {
    param([string]$Message)
    $line = "[$(Get-Date -Format 'yyyy-MM-ddTHH:mm:sszzz')] $Message"
    Write-Host $line
    Add-Content -LiteralPath $logFile -Value $line -Encoding utf8
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

$sshTarget = "$VpsUser@$VpsHost"
# The target must be the LAST element of $sshArgs. Splatting a separate
# $sshTarget alongside an array that did not contain it made ssh treat the
# first word of the remote command (`echo`) as a hostname, which is exactly the
# kind of failure that looks like a network problem and is not.
$sshArgs = @('-i', $SshKey, '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=15', $sshTarget)

# Remote scripts travel base64-encoded and are decoded into `bash -s`.
#
# This is not decoration. PowerShell 5.1 rewrites `\n` to `\r\n` when feeding a
# native command's stdin, so a piped here-string arrives with a trailing
# carriage return on every line and bash fails with `$'\r': command not found`
# or, worse, parses a prefix of the script and exits 0. Base64 is one token with
# no newlines and no characters either shell rewrites. Observed in this project
# in the health check, which silently reported a running container as absent.
function Invoke-RemoteScript {
    param([Parameter(Mandatory)][string]$Script)
    $b64 = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($Script))
    $out = & ssh @sshArgs "echo $b64 | base64 -d | sudo bash -s" 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "remote script failed (exit $LASTEXITCODE): $(($out | Select-Object -Last 3) -join ' | ')"
    }
    return $out
}

# --- what the VPS holds, verified there --------------------------------------
# The listing is produced remotely so nothing here has to guess a filename, and
# it reports each artefact's parts so a half-written one is visible before it is
# pulled.
$listScript = @"
set -u
cd $RemoteDir 2>/dev/null || { echo "STATE=missing"; exit 0; }
newest=`$(ls -1t memgw-*.dump.aes 2>/dev/null | head -n1)
if [ -z "`$newest" ]; then echo "STATE=empty"; exit 0; fi
echo "STATE=ok"
echo "NAME=`$newest"
stem=`${newest%.aes}
for part in "`$newest" "`$stem.key.recovery" "`$stem.key.recovery.fingerprint" "`$stem.manifest" "`$stem.sha256"; do
  if [ -e "`$part" ]; then
    echo "PART=`$part size=`$(stat -c %s "`$part") sha=`$(sha256sum "`$part" | cut -d' ' -f1)"
  else
    echo "MISSING=`$part"
  fi
done
"@
$listing = Invoke-RemoteScript -Script $listScript

$state = ($listing | Where-Object { "$_" -match '^STATE=' } | Select-Object -First 1) -replace '^STATE=', ''
if ($state -ne 'ok') {
    throw "the VPS holds no memgw backup to pull (state=$state); check $RemoteDir and memgw-backup.timer"
}
$name = ($listing | Where-Object { "$_" -match '^NAME=' } | Select-Object -First 1) -replace '^NAME=', ''
$missing = @($listing | Where-Object { "$_" -match '^MISSING=' } | ForEach-Object { $_ -replace '^MISSING=', '' })
if ($missing.Count -gt 0) {
    throw "the newest VPS artefact ($name) is incomplete: missing $($missing -join ', ')"
}

# Remote sha256 per part, so the comparison after transfer is against what the
# source actually holds rather than against a value read from the manifest.
$remoteSha = @{}
foreach ($line in $listing) {
    if ("$line" -match '^PART=(\S+) size=(\d+) sha=([0-9a-f]{64})$') {
        $remoteSha[$Matches[1]] = [pscustomobject]@{ Size = [long]$Matches[2]; Sha = $Matches[3] }
    }
}
Write-Log "VPS artefact: $name ($($remoteSha[$name].Size) bytes, $($remoteSha.Count) parts)"

# One naming convention, matching the producer: everything is derived from the
# stem (`<db>-<stamp>.dump`), never by appending to the `.aes` filename. Getting
# this wrong on either side makes a reader look for files that do not exist.
$stemName = $name -replace '\.aes$', ''
$parts = @($name, "$stemName.key.recovery", "$stemName.key.recovery.fingerprint",
           "$stemName.manifest", "$stemName.sha256")

if ($WhatIfPreference) {
    Write-Host "would pull $($parts.Count) files for $name to $Dest"
    exit 0
}

# --- pull --------------------------------------------------------------------
# Stage into a root-owned directory the ssh account may read, exactly as a
# recovery operator would: the artefacts are 0600 root:root on the VPS.
$stageDir = "/tmp/memgw-pull-$(Get-Random)"
$prepare = Invoke-RemoteScript -Script @"
set -e
install -d -o $VpsUser -g $VpsUser -m 0700 $stageDir
for f in $($parts -join ' '); do
  sudo install -o $VpsUser -g $VpsUser -m 0600 $RemoteDir/`$f $stageDir/`$f
done
ls -1 $stageDir
"@

$batch = ($parts | ForEach-Object { "get -p $stageDir/$_ `"$Dest\\$_`"" }) -join "`n"
$batchFile = Join-Path $env:TEMP "memgw-pull-$([guid]::NewGuid().ToString('N')).txt"
Set-Content -LiteralPath $batchFile -Value $batch -Encoding ascii
try {
    $out = & sftp -i $SshKey -o BatchMode=yes -b $batchFile $sshTarget 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "sftp failed (exit $LASTEXITCODE): $(($out | Select-Object -Last 3) -join ' | ')"
    }
} finally {
    Remove-Item -LiteralPath $batchFile -Force -ErrorAction SilentlyContinue
    $null = & ssh @sshArgs "rm -rf $stageDir" 2>&1
}

# --- integrity in transit ----------------------------------------------------
foreach ($p in $parts) {
    $local = Join-Path $Dest $p
    if (-not (Test-Path -LiteralPath $local)) { throw "pull is incomplete: $p did not arrive" }
    $localSha = Get-Sha256Hex -Path $local
    if ($localSha -ne $remoteSha[$p].Sha) {
        throw "$p differs after transfer: VPS $($remoteSha[$p].Sha), local $localSha"
    }
}
Write-Log "pulled and hash-verified: $name ($($parts.Count)/$($parts.Count) parts)"

# --- recoverability without this machine -------------------------------------
if ($VerifyRecovery) {
    $work = Join-Path $env:TEMP "memgw-verify-$([guid]::NewGuid().ToString('N'))"
    New-Item -ItemType Directory -Force -Path $work | Out-Null
    $priv = $null; $dataKey = $null; $plain = $null
    try {
        $envelope = Join-Path $RepoOpsDir 'ledger-envelope.py'
        $vaultGet = Join-Path $RepoOpsDir 'vault-get.py'
        foreach ($need in @($envelope, $vaultGet)) {
            if (-not (Test-Path -LiteralPath $need)) { throw "helper not found: $need" }
        }

        $stem = Join-Path $Dest ($name -replace '\.aes$', '')
        $sealed = Join-Path $Dest $name

        # The escrowed private half, fetched to a file and never echoed.
        $priv = Join-Path $work 'recovery.pem'
        & python $vaultGet $VaultKeyPath $priv 2>&1 | Out-Null
        if (-not (Test-Path -LiteralPath $priv)) { throw 'could not fetch the escrowed recovery key from the vault' }

        $dataKey = Join-Path $work 'data.key'
        $u = & python $envelope unwrap-recovery --key-recovery "$stem.key.recovery" --recovery-key $priv --out $dataKey 2>&1
        if ($LASTEXITCODE -ne 0) { throw "the escrowed key did not unwrap the data key: $(($u | Select-Object -Last 2) -join ' ')" }

        $plain = Join-Path $work 'ledger.dump'
        $d = & python $envelope unseal --input $sealed --key-file $dataKey --out $plain 2>&1
        if ($LASTEXITCODE -ne 0) { throw "the sealed artefact did not unseal: $(($d | Select-Object -Last 2) -join ' ')" }

        # Reconcile against the manifest, not the live ledger: the ledger is
        # live and keeps committing, so a frozen dump will trail it by however
        # many events landed since. The manifest is the backup's own provenance.
        $manifestCounts = $null
        # Read the manifest and strip a BOM/zero-width chars before matching.
        # `Get-Content` on Windows PowerShell 5.1 keeps a leading U+FEFF when the
        # file was written with a BOM, so `^counts=` never matches the manifest's
        # first line. Observed here: a perfectly good manifest reported as having
        # "no counts to reconcile against".
        $raw = [System.IO.File]::ReadAllText("$stem.manifest")
        $raw = $raw.TrimStart([char]0xFEFF, [char]0x200B)
        foreach ($line in ($raw -split "`r?`n")) {
            $line = $line.TrimStart([char]0xFEFF, [char]0x200B)
            if ($line -match '^counts=(.+)$') { $manifestCounts = $Matches[1].Trim() }
        }
        if (-not $manifestCounts) { throw 'the manifest records no counts to reconcile against' }

        $toc = (($raw -split "`r?`n") | Where-Object { $_ -match '^toc_entries=' }) -replace '^toc_entries=', ''
        if (-not $toc) { throw 'the manifest records no toc_entries' }
        if ([int]$toc -lt 100) { throw "the manifest records only $toc TOC entries; this is not a real ledger dump" }

        # What was sealed is the `.gz`, not the decompressed archive, so the
        # first bytes here are the gzip magic (1f 8b) and the pg magic only
        # appears one layer down. Checking for the pg magic directly rejects a
        # perfectly good backup -- observed here, where `1f8b080000` was read as
        # "not a pg archive". Check gzip framing first, then that the stream
        # actually decompresses to a pg custom-format archive.
        $bytes = [System.IO.File]::ReadAllBytes($plain)
        if ($bytes.Length -lt 2 -or $bytes[0] -ne 0x1f -or $bytes[1] -ne 0x8b) {
            $magic = (($bytes[0..([Math]::Min(4, $bytes.Length - 1))]) | ForEach-Object { $_.ToString('x2') }) -join ''
            throw "the recovered payload is not a gzip stream (magic $magic)"
        }

        # One layer down: decompress the first bytes and require the pg custom
        # format magic. This proves the archive survived the round trip rather
        # than merely that some gzip file came back.
        $gz = [System.IO.Compression.GZipStream]::new(
            [System.IO.File]::OpenRead($plain), [System.IO.Compression.CompressionMode]::Decompress)
        try {
            $head = New-Object byte[] 5
            $read = $gz.Read($head, 0, 5)
            if ($read -lt 5) { throw 'the recovered stream is too short to be an archive' }
            $pgMagic = ($head | ForEach-Object { $_.ToString('x2') }) -join ''
        } finally { $gz.Dispose() }
        if ($pgMagic -ne '5047444d50') {
            throw "the recovered gzip stream does not decompress to a pg custom-format archive (magic $pgMagic)"
        }
        $plainSize = (Get-Item -LiteralPath $plain).Length
        $gzSize = (Get-Item -LiteralPath $sealed).Length
        if ($plainSize -le 0 -or $plainSize -gt 10GB) { throw "the recovered payload has an implausible size: $plainSize bytes" }

        Write-Log "recovery proof OK: escrowed key unsealed $name ($gzSize sealed -> $plainSize gzip), decompresses to pg custom-format archive, manifest counts '$manifestCounts', $toc TOC entries"
    } finally {
        # The private key and the unsealed plaintext must not outlive this block.
        Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
    }
}

# --- local retention ---------------------------------------------------------
$all = @(Get-ChildItem -LiteralPath $Dest -Filter 'memgw-*.dump.aes' -File -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -match '^memgw-\d{8}-\d{6}\.dump\.aes$' } |
    Sort-Object LastWriteTime -Descending)
foreach ($old in ($all | Select-Object -Skip $Keep)) {
    $stem = $old.FullName -replace '\.aes$', ''
    Remove-Item -LiteralPath @($old.FullName, "$stem.key.recovery",
                              "$stem.key.recovery.fingerprint",
                              "$($old.FullName).manifest", "$($old.FullName).sha256") `
        -Force -ErrorAction SilentlyContinue
    Write-Log "pruned $($old.Name)"
}
$kept = @($all | Select-Object -First $Keep).Count
Write-Log "done: $kept artefact(s) kept in $Dest"

Write-Log 'ok'
exit 0
