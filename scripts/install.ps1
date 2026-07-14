<#
.SYNOPSIS
  Download, install, update or remove eneverre from GitHub releases on Windows.

.DESCRIPTION
  The Windows counterpart of scripts/install.sh. It fetches the correct
  windows tarball for the host architecture from the GitHub Releases of
  matiasdelellis/eneverre-server, verifies its SHA-256, and installs
  eneverre.exe. With -InstallService it registers a native Windows service
  with the built-in Service Control Manager (New-Service / sc.exe) - no
  third-party wrapper - and seeds the data directory with a config file and an
  empty cameras.d so the service can start.

  eneverre.exe is service-aware on Windows: under the SCM it reports its state
  and translates a Stop / machine-shutdown control into the same graceful
  shutdown as Ctrl+C, so the in-progress recording segment is finalized
  instead of dropped. See doc\WINDOWS.md for the manual equivalent.

  Requires: Windows 10 1803+ (bundled tar.exe) and PowerShell 5.1+.
  -InstallService and -Uninstall need an elevated (Administrator) shell.

.EXAMPLE
  # Install the latest release to C:\Program Files\Eneverre
  .\install.ps1

.EXAMPLE
  # Install and register the service (run from an elevated shell)
  .\install.ps1 -InstallService

.EXAMPLE
  # Install a specific version (a bare number is accepted: 1.0.0 -> v1.0.0)
  .\install.ps1 -Version v1.0.0

.EXAMPLE
  # Remove the service and the binary (keeps config + data)
  .\install.ps1 -Uninstall
#>

[CmdletBinding()]
param(
    [string]$Version,
    [string]$TargetDir = "$env:ProgramFiles\Eneverre",
    [string]$DataDir   = "$env:ProgramData\Eneverre",
    [string]$AdminPassword,
    [switch]$List,
    [switch]$InstallService,
    [switch]$Force,
    [switch]$NoVerify,
    [switch]$DryRun,
    [switch]$Uninstall,
    [switch]$Yes,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'
# Windows PowerShell 5.1 does not negotiate TLS 1.2 by default; GitHub needs it.
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch {}

# ---- Defaults --------------------------------------------------------------
$Repo        = 'matiasdelellis/eneverre-server'
$Binary      = 'eneverre.exe'
$ServiceName = 'Eneverre'
$ServiceKey  = "HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName"

# ---- Helpers ---------------------------------------------------------------
function Log([string]$Message) { Write-Host "==> $Message" }
function Die([string]$Message) { Write-Error "error: $Message"; exit 1 }
# Run a script block, or just describe it when -DryRun is set.
function Invoke-Step([string]$Description, [scriptblock]$Action) {
    if ($DryRun) { Write-Host "  [dry-run] $Description" } else { & $Action }
}
function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)
}
function Get-Api([string]$Url) {
    Invoke-RestMethod -Uri $Url -Headers @{ 'Accept' = 'application/vnd.github+json'; 'User-Agent' = 'eneverre-install' }
}

if ($Help) { Get-Help -Detailed $PSCommandPath; exit 0 }

# --uninstall is mutually exclusive with the install-only flags (dry-run is
# allowed, to preview what would be removed).
if ($Uninstall -and ($Version -or $List -or $InstallService -or $Force -or $NoVerify)) {
    Die '-Uninstall cannot be combined with other install flags'
}

if (-not (Get-Command tar.exe -ErrorAction SilentlyContinue)) {
    Die 'missing required tool: tar.exe (needs Windows 10 1803+ or Git for Windows)'
}

# ---- Detect architecture ---------------------------------------------------
switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { $Arch = 'amd64' }
    'ARM64' { $Arch = 'arm64' }
    default { Die "unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
}
$Os = 'windows'
Log "platform: $Os/$Arch"

if ($List) {
    Log "recent releases of ${Repo}:"
    (Get-Api "https://api.github.com/repos/$Repo/releases?per_page=10") |
        ForEach-Object { Write-Host "  $($_.tag_name)" }
    exit 0
}

# ---- Uninstall -------------------------------------------------------------
if ($Uninstall) {
    $target  = Join-Path $TargetDir $Binary
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue

    if (-not $service -and -not (Test-Path -LiteralPath $target)) {
        Die "no install found (no $ServiceName service and no $target)"
    }
    if ($service -and -not $DryRun -and -not (Test-Admin)) {
        Die 'removing the service needs an elevated shell - re-run as Administrator'
    }
    if (-not $Yes -and -not $DryRun) {
        $reply = Read-Host "Remove $target and the $ServiceName service? [y/N]"
        if ($reply -notmatch '^[Yy]$') { Write-Host 'aborted'; exit 0 }
    }

    if ($service) {
        if ($service.Status -ne 'Stopped') {
            Log "stopping $ServiceName"
            Invoke-Step "Stop-Service $ServiceName" { Stop-Service -Name $ServiceName -Force }
        }
        Log "removing service $ServiceName"
        Invoke-Step "sc.exe delete $ServiceName" { & sc.exe delete $ServiceName | Out-Null }
    }
    if (Test-Path -LiteralPath $target) {
        Log "removing $target"
        Invoke-Step "remove $target" { Remove-Item -LiteralPath $target -Force }
    }

    Log 'uninstall complete'
    Log "left untouched: $DataDir (config + database + recordings) - remove it manually for a fully clean uninstall"
    exit 0
}

# ---- Resolve version -------------------------------------------------------
# Accept a bare version number (1.0.0) as well as a tag (v1.0.0).
if ($Version -and $Version -match '^[0-9]') { $Version = "v$Version" }
if (-not $Version) {
    Log 'fetching latest release tag from GitHub'
    $Version = (Get-Api "https://api.github.com/repos/$Repo/releases/latest").tag_name
    if (-not $Version) { Die 'could not determine the latest release (GitHub API rate-limited?)' }
}
Log "version: $Version"

# ---- Preflight: permissions ------------------------------------------------
if (-not $DryRun -and $InstallService -and -not (Test-Admin)) {
    Die '-InstallService needs an elevated shell - re-run as Administrator'
}

# ---- Download + verify -----------------------------------------------------
$tarball  = "eneverre-$Version-$Os-$Arch.tar.gz"
$checksum = "$tarball.sha256"
$baseUrl  = "https://github.com/$Repo/releases/download/$Version"
$tmp      = Join-Path ([IO.Path]::GetTempPath()) ("eneverre-" + [IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
    $tarPath = Join-Path $tmp $tarball
    Log "downloading $tarball"
    try {
        Invoke-WebRequest -Uri "$baseUrl/$tarball" -OutFile $tarPath -UseBasicParsing
    } catch {
        Die "could not download $tarball - does $Version ship a $Os/$Arch build? (try: -List)"
    }

    if (-not $NoVerify) {
        Log 'verifying SHA256'
        $sumPath = Join-Path $tmp $checksum
        try {
            Invoke-WebRequest -Uri "$baseUrl/$checksum" -OutFile $sumPath -UseBasicParsing
        } catch {
            Die "could not download the checksum $checksum (use -NoVerify to skip)"
        }
        $expected = ((Get-Content -LiteralPath $sumPath -Raw).Trim() -split '\s+')[0]
        $actual   = (Get-FileHash -LiteralPath $tarPath -Algorithm SHA256).Hash.ToLower()
        if ($expected.ToLower() -ne $actual) { Die "SHA256 mismatch (expected $expected, got $actual)" }
    }

    # ---- Extract -----------------------------------------------------------
    Log 'extracting'
    & tar.exe -xzf $tarPath -C $tmp
    if ($LASTEXITCODE -ne 0) { Die 'tar failed to extract the tarball' }

    # The tarball wraps everything in eneverre-<ver>-<os>-<arch>\.
    $binItem = Get-ChildItem -Path $tmp -Recurse -Filter $Binary -File | Select-Object -First 1
    if (-not $binItem) { Die "expected binary $Binary not found in tarball" }
    $exampleDir = Join-Path $binItem.Directory.FullName 'doc\example'

    # ---- Install binary ----------------------------------------------------
    $target  = Join-Path $TargetDir $Binary
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    $wasRunning = $service -and $service.Status -eq 'Running'

    # A running service holds the .exe open, so an update must stop it first.
    if ($wasRunning -and (Test-Path -LiteralPath $target)) {
        if ($InstallService) {
            Log "stopping $ServiceName to replace the running binary"
            Invoke-Step "Stop-Service $ServiceName" { Stop-Service -Name $ServiceName -Force }
        } else {
            Log "warning: the $ServiceName service is running and holds $target open;"
            Log "         stop it first (Stop-Service $ServiceName) or re-run with -InstallService"
        }
    }

    if (Test-Path -LiteralPath $target) {
        $current = (& $target --version 2>$null) -join ' '
        Log "replacing existing install (current: $current)"
    } else {
        Log 'installing new binary'
    }
    Invoke-Step "install $($binItem.FullName) -> $target" {
        New-Item -ItemType Directory -Path $TargetDir -Force | Out-Null
        Copy-Item -LiteralPath $binItem.FullName -Destination $target -Force
    }
    if (-not $DryRun) {
        Log 'verifying'
        & $target --version
        if ($LASTEXITCODE -ne 0) { Die 'installed binary failed to run' }
    }
    Log 'done'

    if ($wasRunning -and -not $InstallService -and -not $DryRun) {
        Log "note: restart the service to apply the update: Restart-Service $ServiceName"
    }

    # ---- Optional: native Windows service ---------------------------------
    if ($InstallService) {
        $configPath  = Join-Path $DataDir 'eneverre.ini'
        $camerasDir  = Join-Path $DataDir 'cameras.d'
        $dbPath      = Join-Path $DataDir 'eneverre.db'
        $logPath     = Join-Path $DataDir 'eneverre.log'
        $firstInstall = -not (Test-Path -LiteralPath $dbPath)

        # The service runs as LocalSystem by default, so a manual permission
        # check here only proves the installer can write — but a failure
        # usually means the service would fail too, and the script can
        # report it up front instead of leaving an event-log mystery. Check
        # early so we abort with a clear error before seeding the config.
        if (-not $DryRun) {
            if (-not (Test-Path -LiteralPath $DataDir)) {
                try { New-Item -ItemType Directory -Path $DataDir -Force | Out-Null }
                catch { Die "cannot create $DataDir : $($_.Exception.Message)" }
            }
            $probe = Join-Path $DataDir '.eneverre-install-probe'
            try {
                [IO.File]::OpenWrite($probe).Close()
                Remove-Item -LiteralPath $probe -Force -ErrorAction SilentlyContinue
            } catch {
                Die "$DataDir is not writable: $($_.Exception.Message) - the service (LocalSystem) would fail to start. Pick a different -DataDir or fix the ACLs."
            }
        }

        # Seed the data dir so the service can start: config.Load() requires
        # eneverre.ini to exist. Existing config is NEVER overwritten.
        Log "ensuring $camerasDir"
        Invoke-Step "create $camerasDir" { New-Item -ItemType Directory -Path $camerasDir -Force | Out-Null }

        if (Test-Path -LiteralPath $configPath) {
            Log "keeping existing $configPath"
        } elseif (Test-Path -LiteralPath (Join-Path $exampleDir 'eneverre.ini')) {
            Log "seeding $configPath from the example"
            Invoke-Step "copy example eneverre.ini -> $configPath" {
                New-Item -ItemType Directory -Path $DataDir -Force | Out-Null
                Copy-Item -LiteralPath (Join-Path $exampleDir 'eneverre.ini') -Destination $configPath -Force
            }
        } else {
            Die "example config not found in tarball: $exampleDir\eneverre.ini"
        }
        Log "add cameras in $camerasDir (templates in $exampleDir\cameras.d), then restart the service"
        Log "to record, set [media] record_dir to a Windows path in eneverre.ini (see doc\WINDOWS.md)"

        # Paths are passed on the command line (absolute, so the service starts
        # regardless of the working directory). The log file is env-only - the
        # SCM reads per-service vars from the service key's Environment value -
        # so the first-run admin password lands in eneverre.log.
        $binPathName = '"{0}" --config "{1}" --cameras-dir "{2}" --db "{3}" --log-level info' -f `
            $target, $configPath, $camerasDir, $dbPath
        $envValues = @("ENEVERRE_LOG_FILE=$logPath")
        if ($AdminPassword) { $envValues += "ENEVERRE_ADMIN_USER=admin", "ENEVERRE_ADMIN_PASS=$AdminPassword" }

        if ($service -and -not $Force) {
            Log "service $ServiceName already exists (pass -Force to recreate it)"
        } else {
            if ($service) {
                Log "recreating service $ServiceName"
                Invoke-Step "sc.exe delete $ServiceName" {
                    if ((Get-Service -Name $ServiceName).Status -ne 'Stopped') { Stop-Service -Name $ServiceName -Force }
                    & sc.exe delete $ServiceName | Out-Null
                    Start-Sleep -Milliseconds 500
                }
            }
            Log "creating service $ServiceName"
            Invoke-Step "New-Service $ServiceName" {
                New-Service -Name $ServiceName -BinaryPathName $binPathName -DisplayName 'Eneverre NVR API' `
                    -Description 'Vendor-agnostic NVR - records, relays and serves camera streams.' `
                    -StartupType Automatic | Out-Null
            }
            # Per-service environment (REG_MULTI_SZ) the SCM injects at start.
            Invoke-Step "set service environment" {
                New-ItemProperty -Path $ServiceKey -Name Environment -PropertyType MultiString -Value $envValues -Force | Out-Null
            }
            # Restart on failure (and on a non-zero exit), like Restart=on-failure.
            Invoke-Step "configure failure recovery" {
                & sc.exe failure $ServiceName reset= 86400 actions= restart/2000/restart/2000/restart/2000 | Out-Null
                & sc.exe failureflag $ServiceName 1 | Out-Null
            }
        }

        Log "starting $ServiceName"
        Invoke-Step "Start-Service $ServiceName" { Start-Service -Name $ServiceName }

        if (-not $DryRun) {
            Start-Sleep -Seconds 1
            $st = (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue).Status
            Log "service status: $st"
        }

        # On a first install, surface the auto-generated admin password from the
        # log file (unless -AdminPassword set a known one).
        if ($firstInstall -and -not $DryRun -and -not $AdminPassword) {
            $cred = $null
            foreach ($i in 1..5) {
                if (Test-Path -LiteralPath $logPath) {
                    $cred = Select-String -Path $logPath -Pattern 'generated password' -SimpleMatch |
                            Select-Object -First 1 -ExpandProperty Line
                }
                if ($cred) { break }
                Start-Sleep -Seconds 1
            }
            Log 'admin user created on first start with a GENERATED password:'
            if ($cred) {
                Log "  $($cred.Trim())"
            } else {
                Log "  see it with: Select-String -Path `"$logPath`" -Pattern 'generated password'"
            }
            Log 'log in at http://localhost:8080/ and change it immediately'
        }
    }
} finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
