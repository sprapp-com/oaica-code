<#
.SYNOPSIS
    Install, upgrade, or uninstall OAICA on Windows.

.DESCRIPTION
    Downloads and installs OAICA.

    Quick install:

        irm https://oaica.com/install.ps1 | iex

    Specific version:

        $env:OAICA_VERSION="0.5.7"; irm https://oaica.com/install.ps1 | iex

    Custom install directory:

        $env:OAICA_INSTALL_DIR="D:\OAICA"; irm https://oaica.com/install.ps1 | iex

    Uninstall:

        $env:OAICA_UNINSTALL=1; irm https://oaica.com/install.ps1 | iex

    Environment variables:

        OAICA_VERSION       Target version (default: latest stable)
        OAICA_INSTALL_DIR   Custom install directory
        OAICA_UNINSTALL     Set to 1 to uninstall OAICA
        OAICA_DEBUG         Enable verbose output

.EXAMPLE
    irm https://oaica.com/install.ps1 | iex

.EXAMPLE
    $env:OAICA_VERSION = "0.5.7"; irm https://oaica.com/install.ps1 | iex

.LINK
    https://oaica.com
#>

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

# --------------------------------------------------------------------------
# Configuration from environment variables
# --------------------------------------------------------------------------

$Version      = if ($env:OAICA_VERSION) { $env:OAICA_VERSION } else { "" }
$InstallDir   = if ($env:OAICA_INSTALL_DIR) { $env:OAICA_INSTALL_DIR } else { "" }
$Uninstall    = $env:OAICA_UNINSTALL -eq "1"
$DebugInstall = [bool]$env:OAICA_DEBUG

# --------------------------------------------------------------------------
# Constants
# --------------------------------------------------------------------------

# OAICA_DOWNLOAD_URL for developer testing only
$DownloadBaseURL = if ($env:OAICA_DOWNLOAD_URL) { $env:OAICA_DOWNLOAD_URL.TrimEnd('/') } else { "https://oaica.com/download" }

# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------

function Write-Status {
    param([string]$Message)
    if ($DebugInstall) { Write-Host $Message }
}

function Write-Step {
    param([string]$Message)
    if ($DebugInstall) { Write-Host ">>> $Message" -ForegroundColor Cyan }
}

function Update-SessionPath {
    # Update PATH in current session so 'oaica' works immediately
    if ($InstallDir) {
        $oaicaDir = $InstallDir
    } else {
        $oaicaDir = Join-Path $env:LOCALAPPDATA "Programs\OAICA"
    }

    # Add to PATH if not already present
    if (Test-Path $oaicaDir) {
        $currentPath = $env:PATH -split ';'
        if ($oaicaDir -notin $currentPath) {
            $env:PATH = "$oaicaDir;$env:PATH"
            Write-Status "  Added $oaicaDir to session PATH"
        }
    }
}

function Invoke-Download {
    param(
        [string]$Url,
        [string]$OutFile
    )

    Write-Status "  Downloading: $Url"
    try {
        $request = [System.Net.HttpWebRequest]::Create($Url)
        $request.AllowAutoRedirect = $true
        $response = $request.GetResponse()
        $totalBytes = $response.ContentLength
        $stream = $response.GetResponseStream()
        $fileStream = [System.IO.FileStream]::new($OutFile, [System.IO.FileMode]::Create)
        $buffer = [byte[]]::new(65536)
        $totalRead = 0
        $lastUpdate = [DateTime]::MinValue
        $barWidth = 40

        try {
            while (($read = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
                $fileStream.Write($buffer, 0, $read)
                $totalRead += $read

                $now = [DateTime]::UtcNow
                if (($now - $lastUpdate).TotalMilliseconds -ge 250) {
                    if ($totalBytes -gt 0) {
                        $pct = [math]::Min(100.0, ($totalRead / $totalBytes) * 100)
                        $filled = [math]::Floor($barWidth * $pct / 100)
                        $empty = $barWidth - $filled
                        $bar = ('#' * $filled) + (' ' * $empty)
                        $pctFmt = $pct.ToString("0.0")
                        Write-Host -NoNewline "`r$bar ${pctFmt}%"
                    } else {
                        $sizeMB = [math]::Round($totalRead / 1MB, 1)
                        Write-Host -NoNewline "`r${sizeMB} MB downloaded..."
                    }
                    $lastUpdate = $now
                }
            }

            # Final progress update
            if ($totalBytes -gt 0) {
                $bar = '#' * $barWidth
                Write-Host "`r$bar 100.0%"
            } else {
                $sizeMB = [math]::Round($totalRead / 1MB, 1)
                Write-Host "`r${sizeMB} MB downloaded.          "
            }
        } finally {
            $fileStream.Close()
            $stream.Close()
            $response.Close()
        }
    } catch {
        if ($_.Exception -is [System.Net.WebException]) {
            $webEx = [System.Net.WebException]$_.Exception
            if ($webEx.Response -and ([System.Net.HttpWebResponse]$webEx.Response).StatusCode -eq [System.Net.HttpStatusCode]::NotFound) {
                throw "Download failed: not found at $Url"
            }
        }
        if ($_.Exception.InnerException -is [System.Net.WebException]) {
            $webEx = [System.Net.WebException]$_.Exception.InnerException
            if ($webEx.Response -and ([System.Net.HttpWebResponse]$webEx.Response).StatusCode -eq [System.Net.HttpStatusCode]::NotFound) {
                throw "Download failed: not found at $Url"
            }
        }
        throw "Download failed for ${Url}: $($_.Exception.Message)"
    }
}

# --------------------------------------------------------------------------
# Uninstall
# --------------------------------------------------------------------------

function Invoke-Uninstall {
    Write-Step "Uninstalling OAICA"

    # Plain-file install (no Inno Setup registry entry) — remove the
    # install dir directly and strip it from the persistent user PATH.
    $oaicaDir = if ($InstallDir) { $InstallDir } else { Join-Path $env:LOCALAPPDATA "Programs\OAICA" }

    if (-not (Test-Path $oaicaDir)) {
        Write-Host ">>> OAICA is not installed."
        return
    }

    Remove-Item $oaicaDir -Recurse -Force

    $userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
    $newPath = ($userPath -split ';' | Where-Object { $_ -ne $oaicaDir }) -join ';'
    if ($newPath -ne $userPath) {
        [Environment]::SetEnvironmentVariable("PATH", $newPath, "User")
    }

    Write-Host ">>> OAICA has been uninstalled."
}

# --------------------------------------------------------------------------
# Install
# --------------------------------------------------------------------------

function Invoke-Install {
    # OAICA is a thin CLI (talks to api.sprapp.com — OAICA_FORK_PLAN.md
    # option 2), not a GUI desktop app, so unlike upstream Ollama this
    # ships a plain oaica.exe in a zip, not an Inno Setup installer — no
    # Authenticode signature to verify either (no code-signing cert yet;
    # see the TODO in Test-Signature above for when one exists).
    if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") {
        throw "No Windows ARM64 build yet — only amd64."
    }

    if ($Version) {
        $zipUrl = "$DownloadBaseURL/oaica-windows-amd64.zip?version=$Version"
    } else {
        $zipUrl = "$DownloadBaseURL/oaica-windows-amd64.zip"
    }

    Write-Step "Downloading OAICA"
    if (-not $DebugInstall) {
        Write-Host ">>> Downloading OAICA for Windows..."
    }

    $tempZip = Join-Path $env:TEMP "oaica-windows-amd64.zip"
    Invoke-Download -Url $zipUrl -OutFile $tempZip

    $oaicaDir = if ($InstallDir) { $InstallDir } else { Join-Path $env:LOCALAPPDATA "Programs\OAICA" }

    Write-Step "Installing OAICA to $oaicaDir"
    if (-not $DebugInstall) {
        Write-Host ">>> Installing OAICA..."
    }

    $tempExtract = Join-Path $env:TEMP "oaica-extract"
    if (Test-Path $tempExtract) { Remove-Item $tempExtract -Recurse -Force }
    Expand-Archive -Path $tempZip -DestinationPath $tempExtract -Force

    if (-not (Test-Path $oaicaDir)) {
        New-Item -ItemType Directory -Path $oaicaDir -Force | Out-Null
    }
    Copy-Item -Path (Join-Path $tempExtract "bin\oaica.exe") -Destination (Join-Path $oaicaDir "oaica.exe") -Force

    # Cleanup
    Remove-Item $tempZip -Force -ErrorAction SilentlyContinue
    Remove-Item $tempExtract -Recurse -Force -ErrorAction SilentlyContinue

    # Persist PATH for future sessions (user scope, no admin needed)
    $userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
    if ($userPath -notlike "*$oaicaDir*") {
        [Environment]::SetEnvironmentVariable("PATH", "$userPath;$oaicaDir", "User")
        Write-Status "  Added $oaicaDir to persistent user PATH"
    }

    # Update PATH in current session so 'oaica' works immediately
    Write-Step "Updating session PATH"
    Update-SessionPath

    Write-Host ">>> Install complete. Run 'oaica' from the command line."
}

# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------

if ($Uninstall) {
    Invoke-Uninstall
} else {
    Invoke-Install
}
