<#
.SYNOPSIS
Supervises multi-week continuous market-data capture for the TickStream project.

.DESCRIPTION
Runs tickstreamd.exe in a supervised loop, restarting on crashes with exponential backoff.
Ensures disk space never falls below a critical threshold and logs all activity.
Provides console status updates to confirm tape growth.

.EXAMPLE
.\capture.ps1
Runs with default parameters, capturing to C:\tickstream-capture with 1000 depth.

.EXAMPLE
.\capture.ps1 -Dir "D:\capture" -MinFreeGB 100
Runs with custom directory and minimum free space requirement.
#>
param(
    [string]$Dir = "C:\tickstream-capture",
    [int]$Depth = 1000,
    [int]$MinFreeGB = 50,
    [int]$BackoffSec = 5,
    [int]$MaxBackoffSec = 300,
    [switch]$NoBuild
)

$ErrorActionPreference = 'Stop'
$RepoRoot = Split-Path -Path $PSScriptRoot -Parent

# Build binary unless -NoBuild
if (-not $NoBuild) {
    Push-Location -Path $RepoRoot
    try {
        & go build -o bin\tickstreamd.exe .\cmd\tickstreamd
        if ($LASTEXITCODE -ne 0) { throw "Build failed with exit code $LASTEXITCODE" }
    } finally {
        Pop-Location
    }
}

# Create directories
New-Item -ItemType Directory -Path $Dir -Force | Out-Null
New-Item -ItemType Directory -Path "$Dir\logs" -Force | Out-Null

# The daemon is launched by ABSOLUTE path: the build above pushes and pops the
# location, so by the time the loop runs the working directory is wherever the
# operator invoked the script from, not the repo root.
$Exe = Join-Path $RepoRoot 'bin\tickstreamd.exe'
if (-not (Test-Path -LiteralPath $Exe)) { throw "Daemon not found at $Exe (run without -NoBuild)" }

# Check initial free space. Get-PSDrive wants the bare drive letter ("C"), not
# the root path ("C:\") that DirectoryInfo.Root reports.
$DriveName = (Get-Item -LiteralPath $Dir).PSDrive.Name
if (-not $DriveName) { throw "Cannot determine drive for $Dir" }
$FreeSpaceGB = [math]::Round((Get-PSDrive -Name $DriveName).Free / 1GB, 2)
if ($FreeSpaceGB -lt $MinFreeGB) {
    throw "Initial free space $FreeSpaceGB GB below minimum $MinFreeGB GB"
}

# Log file handles
$SupervisorLog = "$Dir\logs\supervisor.log"
Add-Content -Path $SupervisorLog -Value "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') supervisor started dir=$Dir depth=$Depth"

$CurrentBackoff = $BackoffSec
$RunCount = 0

# Supervision loop
while ($true) {
    # Re-check free space
    $FreeSpaceGB = [math]::Round((Get-PSDrive -Name $DriveName).Free / 1GB, 2)
    if ($FreeSpaceGB -lt $MinFreeGB) {
        # Stop on low disk to prevent OS crash
        Add-Content -Path $SupervisorLog -Value "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') STOPPING: free space $FreeSpaceGB GB below $MinFreeGB GB"
        break
    }

    # Launch tickstreamd.exe
    $Timestamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $LogFile = "$Dir\logs\tickstreamd-$Timestamp.log"
    $ErrLog = "$Dir\logs\tickstreamd-$Timestamp.err.log"
    $StartTime = Get-Date

    # -flag=value form: Start-Process drops bare empty-string arguments and
    # splits unquoted paths containing spaces.
    $Process = Start-Process -FilePath $Exe -ArgumentList @(
        "-capture-dir=`"$Dir`""
        "-depth=$Depth"
    ) -NoNewWindow -Wait -PassThru -RedirectStandardOutput $LogFile -RedirectStandardError $ErrLog

    $ExitCode = $Process.ExitCode
    $RunDuration = (Get-Date) - $StartTime

    # Log run result
    Add-Content -Path $SupervisorLog -Value "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') exit=$ExitCode duration=$($RunDuration.ToString('hh\:mm\:ss'))"

    # Backoff logic
    if ($RunDuration.TotalMinutes -gt 10) {
        # Long run suggests transient issue, reset backoff
        $CurrentBackoff = $BackoffSec
    } else {
        $CurrentBackoff = [math]::Min($CurrentBackoff * 2, $MaxBackoffSec)
    }

    # Console status
    $GobFiles = Get-ChildItem -Path $Dir -Filter "*.gob.gz" -Recurse -ErrorAction SilentlyContinue
    $TotalSizeGB = [math]::Round((Get-ChildItem -Path $Dir -Recurse -ErrorAction SilentlyContinue | Measure-Object -Property Length -Sum).Sum / 1GB, 2)
    Write-Host "$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') FreeGB=$FreeSpaceGB GobCount=$($GobFiles.Count) TotalSizeGB=$TotalSizeGB"

    # Sleep before retry
    Start-Sleep -Seconds $CurrentBackoff
}