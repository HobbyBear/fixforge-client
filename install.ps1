param(
  [string]$Repo = $env:FIXFORGE_CLIENT_GITHUB_REPO,
  [string]$Version = $env:FIXFORGE_CLIENT_VERSION,
  [string]$InstallDir = $env:FIXFORGE_CLIENT_INSTALL_DIR,
  [Parameter(ValueFromRemainingArguments = $true)]
  [string[]]$ClientArgs
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($Repo)) {
  $Repo = "HobbyBear/fixforge-client"
}
if ([string]::IsNullOrWhiteSpace($Version)) {
  $Version = "latest"
}
if ([string]::IsNullOrWhiteSpace($InstallDir)) {
  $InstallDir = Join-Path $env:LOCALAPPDATA "FixForge\bin"
}

function Normalize-Repo([string]$Value) {
  $Value = $Value.Trim()
  $Value = $Value -replace '^https://github\.com/', ''
  $Value = $Value -replace '^http://github\.com/', ''
  $Value = $Value -replace '^git@github\.com:', ''
  $Value = $Value -replace '\.git$', ''
  return $Value.Trim('/')
}

function Resolve-Arch {
  switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { return "amd64" }
    "ARM64" { return "arm64" }
    default { throw "unsupported arch: $env:PROCESSOR_ARCHITECTURE" }
  }
}

function Add-ToUserPath([string]$Dir) {
  $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
  $parts = @()
  if (-not [string]::IsNullOrWhiteSpace($userPath)) {
    $parts = $userPath.Split(';') | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
  }
  if ($parts -notcontains $Dir) {
    $newPath = (($parts + $Dir) -join ';')
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    Write-Host "Added $Dir to user PATH. Open a new terminal for it to take effect."
  }
  if (($env:Path.Split(';') | Where-Object { $_ -eq $Dir }).Count -eq 0) {
    $env:Path = "$Dir;$env:Path"
  }
}

$Repo = Normalize-Repo $Repo
if ($Version -eq "latest") {
  $latest = Invoke-RestMethod -Headers @{ Accept = "application/vnd.github+json" } -Uri "https://api.github.com/repos/$Repo/releases/latest"
  $Version = $latest.tag_name
}
if ([string]::IsNullOrWhiteSpace($Version)) {
  throw "failed to resolve fixforge-client version"
}

$arch = Resolve-Arch
$asset = "fixforge-client_${Version}_windows_${arch}.zip"
$baseUrl = "https://github.com/$Repo/releases/download/$Version"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("fixforge-client-" + [Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

try {
  $archive = Join-Path $tmp $asset
  Write-Host "Downloading $asset from $Repo..."
  Invoke-WebRequest -Uri "$baseUrl/$asset" -OutFile $archive

  try {
    $checksums = Join-Path $tmp "checksums.txt"
    Invoke-WebRequest -Uri "$baseUrl/checksums.txt" -OutFile $checksums
    $line = Get-Content $checksums | Where-Object { $_ -match "\s\*?$([regex]::Escape($asset))$" } | Select-Object -First 1
    if ($line) {
      $expected = ($line -split '\s+')[0].ToLowerInvariant()
      $actual = (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant()
      if ($expected -ne $actual) {
        throw "checksum mismatch for $asset"
      }
    }
  } catch {
    if ($_.Exception.Message -like "*checksum mismatch*") {
      throw
    }
  }

  Expand-Archive -Path $archive -DestinationPath $tmp -Force
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  $target = Join-Path $InstallDir "fixforge-client.exe"
  Copy-Item -Path (Join-Path $tmp "fixforge-client.exe") -Destination $target -Force
  Add-ToUserPath $InstallDir
  Write-Host "Installed fixforge-client $Version to $target"

  if ($ClientArgs -and $ClientArgs.Count -gt 0) {
    & $target @ClientArgs
  }
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
