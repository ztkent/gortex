<#
.SYNOPSIS
    Gortex one-line installer for Windows (PowerShell).

.DESCRIPTION
    Downloads the signed Windows release archive, verifies its SHA-256
    checksum, installs the self-contained gortex.exe, and puts the install
    directory on the user PATH.

    Usage:
        irm https://get.gortex.dev/install.ps1 | iex

    Or, from a checkout:
        powershell -ExecutionPolicy Bypass -File scripts/install.ps1

    Configuration via environment variables (all optional):
        GORTEX_VERSION        Release tag to install ("latest" or "v0.15.0")
        GORTEX_INSTALL_DIR    Install directory (default: %LOCALAPPDATA%\Programs\gortex)
        GORTEX_NO_VERIFY      Set to skip the SHA-256 checksum verification
        GORTEX_NO_PATH        Set to skip the user PATH update
        GORTEX_FORCE          Set to overwrite an existing binary without backup
        GORTEX_DOWNLOAD_BASE  Override the release download base URL (for testing)
#>

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo        = 'zzet/gortex'
$BinName     = 'gortex.exe'
$DownloadBase = if ($env:GORTEX_DOWNLOAD_BASE) { $env:GORTEX_DOWNLOAD_BASE } `
                else { "https://github.com/$Repo/releases" }

function Write-Info($msg) { Write-Host "==> $msg" -ForegroundColor Blue }
function Write-Ok($msg)   { Write-Host " ok  $msg" -ForegroundColor Green }
function Write-Warn($msg) { Write-Host "  !  $msg" -ForegroundColor Yellow }
function Die($msg)        { Write-Host "  x  $msg" -ForegroundColor Red; exit 1 }

function Get-Arch {
    # We publish a single windows/amd64 archive. Windows on ARM runs x64
    # binaries transparently under emulation, so amd64 is the right asset
    # everywhere except genuine 32-bit hosts.
    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'amd64' }
        'x86'   { Die 'unsupported architecture: x86 (64-bit Windows required)' }
        default { Die "unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
    }
}

function Add-ToUserPath($dir) {
    $current = [Environment]::GetEnvironmentVariable('Path', 'User')
    $parts = @()
    if ($current) { $parts = $current -split ';' | Where-Object { $_ -ne '' } }
    if ($parts -contains $dir) {
        Write-Ok "$dir already on the user PATH"
        return
    }
    $updated = (($parts + $dir) -join ';')
    [Environment]::SetEnvironmentVariable('Path', $updated, 'User')
    # Refresh the current session so the version banner below resolves.
    $env:Path = "$env:Path;$dir"
    Write-Ok "added $dir to the user PATH (open a new shell to pick it up)"
}

function Main {
    $arch    = Get-Arch
    $version = if ($env:GORTEX_VERSION) { $env:GORTEX_VERSION } else { 'latest' }
    $installDir = if ($env:GORTEX_INSTALL_DIR) { $env:GORTEX_INSTALL_DIR } `
                  else { Join-Path $env:LOCALAPPDATA 'Programs\gortex' }

    Write-Host ''
    Write-Host 'Gortex installer' -ForegroundColor White
    Write-Host "  os:      windows"
    Write-Host "  arch:    $arch"
    Write-Host "  version: $version"
    Write-Host "  target:  $installDir\$BinName"
    Write-Host ''

    $asset = "gortex_windows_${arch}.zip"
    if ($version -eq 'latest') {
        $baseUrl = "$DownloadBase/latest/download"
    } else {
        $tag = if ($version.StartsWith('v')) { $version } else { "v$version" }
        $baseUrl = "$DownloadBase/download/$tag"
    }
    $assetUrl     = "$baseUrl/$asset"
    $checksumsUrl = "$baseUrl/checksums.txt"

    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("gortex-install-" + [System.Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp -Force | Out-Null
    try {
        $zipPath = Join-Path $tmp $asset
        Write-Info "downloading $asset"
        try {
            Invoke-WebRequest -Uri $assetUrl -OutFile $zipPath -UseBasicParsing
        } catch {
            Die "download failed: $assetUrl`n  $($_.Exception.Message)"
        }

        if (-not $env:GORTEX_NO_VERIFY) {
            Write-Info 'downloading checksums.txt'
            $checksumsPath = Join-Path $tmp 'checksums.txt'
            try {
                Invoke-WebRequest -Uri $checksumsUrl -OutFile $checksumsPath -UseBasicParsing
                # checksums.txt is `<sha256>  <filename>` per goreleaser default.
                $expected = $null
                foreach ($line in Get-Content $checksumsPath) {
                    $cols = $line -split '\s+'
                    if ($cols.Count -ge 2 -and ($cols[1] -eq $asset -or $cols[1] -eq "*$asset")) {
                        $expected = $cols[0]
                        break
                    }
                }
                if (-not $expected) {
                    Write-Warn "checksums.txt did not contain $asset; skipping verification"
                } else {
                    $actual = (Get-FileHash -Path $zipPath -Algorithm SHA256).Hash.ToLower()
                    if ($actual -ne $expected.ToLower()) {
                        Die "checksum mismatch on $asset`n  expected: $expected`n  actual:   $actual"
                    }
                    Write-Ok "sha256 verified ($asset)"
                }
            } catch {
                Write-Warn 'could not fetch checksums.txt; skipping verification'
            }
        } else {
            Write-Warn 'verification disabled (GORTEX_NO_VERIFY)'
        }

        Write-Info 'extracting'
        $staging = Join-Path $tmp 'extract'
        Expand-Archive -Path $zipPath -DestinationPath $staging -Force
        $extracted = Join-Path $staging $BinName
        if (-not (Test-Path $extracted)) {
            Die "archive did not contain a $BinName binary"
        }

        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
        $target = Join-Path $installDir $BinName
        if ((Test-Path $target) -and (-not $env:GORTEX_FORCE)) {
            $backup = "$target.previous"
            Write-Info "backing up existing binary to $backup"
            Move-Item -Path $target -Destination $backup -Force
        }
        # gortex.exe is a single self-contained binary — the mingw C/C++
        # runtime is statically linked into it — so install is a one-file
        # copy with nothing else to place beside it.
        Copy-Item -Path $extracted -Destination $target -Force
        Write-Ok "installed $target"

        if (-not $env:GORTEX_NO_PATH) {
            Add-ToUserPath $installDir
        }

        # If a daemon is already running an older binary, restart it onto
        # the new one. Best-effort — never block the install on this.
        & $target daemon status *> $null
        if ($LASTEXITCODE -eq 0) {
            Write-Info 'restarting running daemon onto new binary'
            & $target daemon restart *> $null
            if ($LASTEXITCODE -ne 0) {
                Write-Warn "daemon restart failed; run 'gortex daemon restart' manually"
            }
        }

        $versionOut = (& $target version) 2>$null
        if ($versionOut) { Write-Ok "$versionOut" }

        Write-Host ''
        Write-Host 'Next steps:' -ForegroundColor White
        Write-Host "  - gortex install   one-time machine setup (MCP, skills, slash commands)"
        Write-Host "  - gortex init      run inside a repo to wire up your AI assistant"
        Write-Host ''
        Write-Host "Docs: https://github.com/$Repo"
        Write-Host ''
    }
    finally {
        Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Main
