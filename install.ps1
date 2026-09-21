# Installs clawdh for the current user: no system directories, only a
# per-user install dir and a per-user PATH entry (HKCU, not HKLM). The
# default install needs no admin and prompts for none — elevation is
# requested only if CLAWDH_INSTALL_DIR points somewhere this account
# cannot write, and only after Windows has actually refused. Safe to
# pipe straight into a normal (non-elevated) PowerShell prompt:
#
#   irm https://raw.githubusercontent.com/Saif0089/clawdh/main/install.ps1 | iex
$ErrorActionPreference = "Stop"

$Repo = "Saif0089/clawdh"

$arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture) {
  "Arm64"   { "arm64" }
  default   { "amd64" }
}

$version = if ($env:CLAWDH_VERSION) { $env:CLAWDH_VERSION } else { "latest" }
$asset = "clawdh_windows_$arch.exe"
if ($version -eq "latest") {
  $url = "https://github.com/$Repo/releases/latest/download/$asset"
} else {
  $url = "https://github.com/$Repo/releases/download/$version/$asset"
}

# Elevation is never needed for the install this script is designed for:
# %LOCALAPPDATA% and a HKCU PATH entry both belong to the user. But the
# directory is overridable, and an administrator pointing CLAWDH_INSTALL_DIR
# at Program Files — or an enterprise image that pre-creates it — leaves a
# location this account cannot write. Rather than fail there, ask for
# rights, and only once Windows has actually refused. Nothing below
# prompts on an ordinary install.
function Invoke-Elevated([string]$script) {
  # -EncodedCommand rather than quoting: install paths carry spaces as a
  # matter of course (C:\Users\John Smith\...) and apostrophes are legal
  # in a Windows user name, so base64 removes the quoting question
  # instead of answering it by inspection.
  $enc = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($script))
  $p = Start-Process powershell -Verb RunAs -Wait -PassThru -WindowStyle Hidden `
        -ArgumentList '-NoProfile', '-NonInteractive', '-EncodedCommand', $enc
  if ($p.ExitCode -ne 0) {
    throw "this location needs administrator rights, and the prompt was declined or the elevated step failed (exit $($p.ExitCode))"
  }
}

function Test-AccessDenied($errorRecord) {
  # Only "Windows refused for want of rights". A missing parent, a full
  # disk or a locked file are not fixed by elevation, and prompting for
  # them is noise the user has to dismiss.
  if ($errorRecord.Exception -is [System.UnauthorizedAccessException]) { return $true }
  return $errorRecord.CategoryInfo.Category -eq 'PermissionDenied'
}

function ps1Quote([string]$s) { "'" + $s.Replace("'", "''") + "'" }

$installDir = if ($env:CLAWDH_INSTALL_DIR) { $env:CLAWDH_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "clawdh\bin" }
try {
  New-Item -ItemType Directory -Force -Path $installDir | Out-Null
} catch {
  if (-not (Test-AccessDenied $_)) { throw }
  Write-Host "$installDir needs administrator rights to create - prompting..."
  Invoke-Elevated "New-Item -ItemType Directory -Force -Path $(ps1Quote $installDir) | Out-Null"
}
$dest = Join-Path $installDir "clawdh.exe"

Write-Host "Downloading clawdh (windows/$arch)..."

# Stop a previous install before overwriting its exe. Windows locks a
# running executable's file, so without this every upgrade fails with
# "the process cannot access the file because it is being used by
# another process".
if (Test-Path $dest) {
  try { & $dest stop | Out-Null } catch { }
  Start-Sleep -Milliseconds 500
}

# Download into TEMP, not next to $dest: when the install directory is
# one this account cannot write, putting the download there fails before
# the move is ever reached, and the elevation below would never get a
# chance to help. TEMP always belongs to the user.
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) "clawdh-$([guid]::NewGuid().ToString('N')).download"
# Retry a transient CDN hiccup (a 502/504 or a dropped connection) instead of
# failing the install on the first blip. A loop rather than -MaximumRetryCount,
# which Windows PowerShell 5.1 does not have. -UseBasicParsing keeps 5.1 off the
# legacy IE engine.
$attempt = 0
while ($true) {
  try { Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing; break }
  catch {
    $attempt++
    if ($attempt -ge 5) { throw }
    Start-Sleep -Seconds 2
  }
}

# Verify against the checksums published alongside the binary.
try {
  $sumsUrl = ($url -replace '/[^/]+$', '/checksums.txt')
  $sums = (Invoke-WebRequest -Uri $sumsUrl).Content
  $line = ($sums -split "`n" | Where-Object { $_ -match [regex]::Escape($asset) + '\s*$' } | Select-Object -First 1)
  if ($line) {
    $expected = ($line -split '\s+')[0]
    $actual = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash.ToLower()
    if ($expected.ToLower() -ne $actual) {
      Remove-Item $tmp -Force
      throw "checksum mismatch for $asset (expected $expected, got $actual)"
    }
  }
} catch {
  # A release without checksums.txt is fine; anything else is not.
  # Windows PowerShell 5.1 raises WebException here while PowerShell 7
  # raises HttpResponseException, and catching only the former made a
  # missing checksums.txt abort the whole install under $ErrorActionPreference = "Stop".
  $type = $_.Exception.GetType().FullName
  if ($type -ne "System.Net.WebException" -and $type -ne "Microsoft.PowerShell.Commands.HttpResponseException") {
    throw
  }
}

try {
  Move-Item -Force -Path $tmp -Destination $dest
} catch {
  if (-not (Test-AccessDenied $_)) { throw }
  Write-Host "$dest needs administrator rights to write - prompting..."
  Invoke-Elevated "Move-Item -Force -LiteralPath $(ps1Quote $tmp) -Destination $(ps1Quote $dest)"
  if (-not (Test-Path $dest)) { throw "the elevated step reported success but $dest is not there" }
}
Remove-Item $tmp -Force -ErrorAction SilentlyContinue

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not $userPath) { $userPath = "" }
if ($userPath.Split(";") -notcontains $installDir) {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$installDir", "User")
  $env:Path = "$env:Path;$installDir"
  Write-Host "Added $installDir to your user PATH (open a new terminal for it to show up everywhere)."
}

Write-Host "Installed $dest"

# Cross over from a previous ccam install: let the old binary uninstall itself
# (it stops its service, strips its shell block, and removes itself), so it
# doesn't leave a second daemon fighting clawdh for the port. Account data is
# left in place for clawdh to import on first run. Best-effort.
$oldCcam = @(
  (Join-Path $env:LOCALAPPDATA "ccam\bin\ccam.exe"),
  (Join-Path $installDir "ccam.exe")
) | Select-Object -Unique
foreach ($old in $oldCcam) {
  if (Test-Path $old) {
    Write-Host "Removing the previous ccam install..."
    try { & $old uninstall | Out-Null } catch { }
  }
}

Write-Host ""
& $dest install

# An invite link makes install and join one paste: the invite page shows
#   $env:CLAWDH_JOIN = "https://panel/i/<code>"; irm ... | iex
# and clawdh joins right after it starts.
if ($env:CLAWDH_JOIN) {
  Write-Host ""
  & $dest join $env:CLAWDH_JOIN
}
