<#
.SYNOPSIS
    Pull the VPS PostgreSQL backup staging directory off-host, verify it, seal
    it with DPAPI and apply retention.

.DESCRIPTION
    The VPS half (rag-pg-backup.timer -> pg-stage-backup.sh) writes a compressed
    dump, a checksum, a manifest and the WAL archive into
    /opt/rag-backup/stage. This script is the other half: it pulls that staging
    directory to D:\memgw-backups\vps, verifies every dump's sha256 against the
    checksum the VPS wrote, seals it, and prunes.

    Why a pull and not a push
    --------------------------
    A pull initiated from the Windows side survives a compromised VPS: the VPS
    cannot reach out and overwrite the backup host's history. A push would let
    the compromised host write whatever it liked into the backup location.

    What it verifies, and why
    -------------------------
    A backup nobody has verified is a hypothesis. The checksum is computed ON
    THE VPS at backup time and re-computed HERE after transfer; a mismatch means
    the copy is not the backup and the run fails loudly. The manifest travels
    with the data so the sealed copy carries its own provenance.

    Retention
    ---------
    Keeps the newest $Keep sealed dumps (default 4). The unsealed staging copy
    is removed after sealing so plaintext never persists.

.PARAMETER Keep
    Number of sealed dumps to retain (default 4).

.PARAMETER SkipTransfer
    Verify and seal what is already staged locally (no network).

.NOTES
    Read-only with respect to the VPS: it only reads. Touches no protected
    resource.
#>

[CmdletBinding()]
param(
    [int]$Keep = 4,
    [switch]$SkipTransfer,
    # Only pull/verify/seal artefacts for this database (e.g. 'service_charge').
    # Default: every staged database. Useful when one dump is very large and an
    # operator wants to exercise or fetch one target without moving the rest --
    # and it keeps a verification run from depending on the biggest dump.
    [string]$Database = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# ProtectedData (DPAPI) lives in System.Security; load it explicitly rather
# than relying on the host having it pre-loaded.
Add-Type -AssemblyName System.Security

$VpsHost = '168.110.218.207'
$VpsUser = 'ubuntu'
$SshKey  = "$env:USERPROFILE\.ssh\id_oracle"
$RemoteStage = '/opt/rag-backup/stage/'
$LocalRoot = 'D:\memgw-backups\vps'
$LocalStage = Join-Path $LocalRoot 'stage'
$SealedDir  = Join-Path $LocalRoot 'sealed'
$LogFile    = Join-Path $LocalRoot 'pull.log'
# sftp glob prefix: empty for every database, `<db>-` for one.
$globPrefix = if ($Database) { "$Database-" } else { '' }

New-Item -ItemType Directory -Force -Path $LocalStage, $SealedDir | Out-Null

function Write-Log {
    param([string]$Message)
    $line = "[$(Get-Date -Format 'yyyy-MM-ddTHH:mm:ssK')] $Message"
    Write-Host $line
    Add-Content -LiteralPath $LogFile -Value $line -Encoding utf8
}

# Get-FileHash is not present in every PowerShell this runs under (it is absent
# in the constrained host used for verification), so compute the SHA-256 with
# .NET directly. Same algorithm, no cmdlet dependency.
function Get-Sha256Hex {
    param([string]$Path)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $fs = [System.IO.File]::OpenRead($Path)
        try { return ([BitConverter]::ToString($sha.ComputeHash($fs)) -replace '-', '').ToLowerInvariant() }
        finally { $fs.Dispose() }
    } finally { $sha.Dispose() }
}

$ErrorActionPreference = 'Continue'
$failures = 0

# --- transfer --------------------------------------------------------------
if (-not $SkipTransfer) {
    Write-Log "pulling $RemoteStage -> $LocalStage"
    # A single sftp session with a batch of get commands. -p preserves mtimes so
    # a future incremental pull can skip unchanged files; the dump names carry
    # the timestamp, so a re-pull of an existing name is idempotent.
    $batch = @(
        "lcd `"$LocalStage`""
        "get -p ${RemoteStage}$($globPrefix)*.dump.gz"
        "get -p ${RemoteStage}$($globPrefix)*.sha256"
        # Per-dump manifests, so a sealed artefact carries its OWN provenance
        # (database, bytes, sha256, taken_at) instead of whichever dump
        # happened to be newest at seal time.
        "get -p ${RemoteStage}$($globPrefix)*.dump.gz.manifest"
    ) -join "`n"
    if (-not $Database) { $batch += "`nget -p ${RemoteStage}latest.manifest" }
    $batchFile = Join-Path $env:TEMP "memgw-pull-$([guid]::NewGuid().ToString('N')).txt"
    Set-Content -LiteralPath $batchFile -Value $batch -Encoding ascii
    try {
        $out = & sftp -i $SshKey -o BatchMode=yes -b $batchFile "$VpsUser@$VpsHost" 2>&1
        $rc = $LASTEXITCODE
        if ($rc -ne 0) {
            # sftp returns non-zero when a glob matches nothing, which is normal
            # on a day with no new dump; only surface it if nothing arrived.
            Write-Log "sftp exit $rc ($(($out | Select-Object -Last 2) -join ' | '))"
        }
    } finally {
        Remove-Item -LiteralPath $batchFile -Force -ErrorAction SilentlyContinue
    }
}

# --- verify every dump against the checksum the VPS wrote -------------------
$dumps = @(Get-ChildItem -LiteralPath $LocalStage -Filter '*.dump.gz' -ErrorAction SilentlyContinue | Sort-Object LastWriteTime)
if ($dumps.Count -eq 0) {
    Write-Log 'no dumps staged; nothing to seal'
} else {
    foreach ($d in $dumps) {
        $sumFile = "$($d.FullName).sha256"
        if (-not (Test-Path -LiteralPath $sumFile)) {
            Write-Log "FAIL $($d.Name): no checksum file beside it"
            $failures++
            continue
        }
        $expected = ((Get-Content -LiteralPath $sumFile -Raw) -split '\s+')[0].Trim().ToLowerInvariant()
        $actual = Get-Sha256Hex -Path $d.FullName
        if ($expected -ne $actual) {
            Write-Log "FAIL $($d.Name): sha256 mismatch (expected $expected, got $actual)"
            $failures++
            continue
        }
        Write-Log "ok $($d.Name) sha256=$actual"
    }
}

# --- seal unsealed, verified dumps -----------------------------------------
function Seal-One {
    param([System.IO.FileInfo]$Dump)
    $stamp = ($Dump.BaseName -replace '\.dump$', '')
    $sealed = Join-Path $SealedDir "$stamp.aes"
    if (Test-Path -LiteralPath $sealed) { return }

    $entropy = [System.Text.Encoding]::UTF8.GetBytes('enowx-rag axonhub ledger backup v1')
    $key = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($key)
    $sealedKey = [System.Security.Cryptography.ProtectedData]::Protect(
        $key, $entropy, [System.Security.Cryptography.DataProtectionScope]::CurrentUser)
    [System.IO.File]::WriteAllBytes("${sealed}.key.dpapi", $sealedKey)

    $iv = New-Object byte[] 16
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($iv)
    [System.IO.File]::WriteAllBytes("${sealed}.iv", $iv)

    $aes = [System.Security.Cryptography.Aes]::Create()
    $aes.KeySize = 256
    $aes.Mode = [System.Security.Cryptography.CipherMode]::CBC
    $aes.Padding = [System.Security.Cryptography.PaddingMode]::PKCS7
    $aes.Key = $key
    $aes.IV = $iv
    $dec = $aes.CreateEncryptor()

    $in  = [System.IO.File]::OpenRead($Dump.FullName)
    $out = [System.IO.File]::Create($sealed)
    $cs  = New-Object System.Security.Cryptography.CryptoStream($out, $dec, [System.Security.Cryptography.CryptoStreamMode]::Write)
    try { $in.CopyTo($cs, 1048576) } finally { $cs.Dispose(); $in.Dispose(); $out.Dispose() }

    # Carry the dump's OWN manifest beside the sealed copy so provenance
    # travels with it. The per-dump manifest the VPS writes is authoritative;
    # `latest.manifest` is only a fallback for a dump staged by an older
    # revision of the script that did not write per-dump manifests, and using
    # it blind can attach another database's identity to this artefact.
    $ownManifest = "$($Dump.FullName).manifest"
    if (Test-Path -LiteralPath $ownManifest) {
        Copy-Item -LiteralPath $ownManifest -Destination "${sealed}.manifest" -Force
    } else {
        Copy-Item -LiteralPath (Join-Path $LocalStage 'latest.manifest') -Destination "${sealed}.manifest" -Force -ErrorAction SilentlyContinue
        Write-Log "warn ${stamp}: no per-dump manifest; sealed with latest.manifest"
    }
    "$(Get-Sha256Hex -Path $sealed)  $(Split-Path -Leaf $sealed)" |
        Set-Content -LiteralPath "${sealed}.sha256" -Encoding utf8
    Write-Log "sealed $stamp -> $(Split-Path -Leaf $sealed) ($((Get-Item $sealed).Length) bytes)"
}

foreach ($d in $dumps) {
    # Only seal a dump whose checksum verified (or has no checksum file to
    # contradict it, which the verify step already failed). A failure on one
    # dump must not stop the others from being sealed: a broken small test
    # dump would otherwise block the production one.
    $sumFile = "$($d.FullName).sha256"
    $verified = $true
    if (Test-Path -LiteralPath $sumFile) {
        $expected = ((Get-Content -LiteralPath $sumFile -Raw) -split '\s+')[0].Trim().ToLowerInvariant()
        $verified = (Get-Sha256Hex -Path $d.FullName) -eq $expected
    } else {
        $verified = $false
    }
    if ($verified) {
        Seal-One -Dump $d
    }
}

# --- retention -------------------------------------------------------------
$sealedFiles = @(Get-ChildItem -LiteralPath $SealedDir -Filter '*.aes' | Sort-Object LastWriteTime -Descending)
if ($sealedFiles.Count -gt $Keep) {
    $sealedFiles | Select-Object -Skip $Keep | ForEach-Object {
        Remove-Item -LiteralPath $_.FullName, "$($_.FullName).key.dpapi", "$($_.FullName).iv", "$($_.FullName).sha256", "$($_.FullName).manifest" -Force -ErrorAction SilentlyContinue
        Write-Log "pruned sealed $($_.Name)"
    }
}

# Remove the plaintext staging copy now that it is sealed.
Get-ChildItem -LiteralPath $LocalStage -Filter '*.dump.gz' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue

$kept = @(Get-ChildItem -LiteralPath $SealedDir -Filter '*.aes').Count
Write-Log "done: $kept sealed dump(s) kept, $failures failure(s)"
if ($failures -gt 0) { exit 2 }
exit 0
