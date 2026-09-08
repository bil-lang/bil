# Installs the bil CLI. Usage: irm https://bil-lang.org/install.ps1 | iex
#
# Env overrides:
#   $env:BIL_VERSION      release tag to install (default: latest)
#   $env:BIL_INSTALL_DIR  directory to install the bil binary into (default: $env:LOCALAPPDATA\bil\bin)

$ErrorActionPreference = "Stop"

$repo = "bil-lang/bil"
$installDir = if ($env:BIL_INSTALL_DIR) { $env:BIL_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "bil\bin" }
$version = if ($env:BIL_VERSION) { $env:BIL_VERSION } else { "latest" }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
	"AMD64" { "amd64" }
	"ARM64" { "arm64" }
	default {
		Write-Error "bil: unsupported architecture: $env:PROCESSOR_ARCHITECTURE"
		exit 1
	}
}

if ($version -eq "latest") {
	$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest"
	$version = $release.tag_name
	if (-not $version) {
		Write-Error "bil: couldn't resolve the latest release version"
		exit 1
	}
}

$archive = "bil_${version}_windows_${arch}.zip"
$checksums = "bil_${version}_checksums.txt"
$baseUrl = "https://github.com/$repo/releases/download/$version"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
	Write-Host "bil: downloading $archive ($version)..."
	Invoke-WebRequest -Uri "$baseUrl/$archive" -OutFile (Join-Path $tmp $archive)
	Invoke-WebRequest -Uri "$baseUrl/$checksums" -OutFile (Join-Path $tmp $checksums)

	Write-Host "bil: verifying checksum..."
	$checksumsContent = Get-Content (Join-Path $tmp $checksums)
	$expectedLine = $checksumsContent | Where-Object { $_ -match [regex]::Escape($archive) }
	if (-not $expectedLine) {
		Write-Error "bil: no checksum entry found for $archive"
		exit 1
	}
	$expected = ($expectedLine -split '\s+')[0]
	$actual = (Get-FileHash -Path (Join-Path $tmp $archive) -Algorithm SHA256).Hash.ToLower()
	if ($expected -ne $actual) {
		Write-Error "bil: checksum mismatch for $archive (expected $expected, got $actual)"
		exit 1
	}

	Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force

	New-Item -ItemType Directory -Path $installDir -Force | Out-Null
	Move-Item -Path (Join-Path $tmp "bil.exe") -Destination (Join-Path $installDir "bil.exe") -Force

	Write-Host "bil: installed to $installDir\bil.exe"

	$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
	if (-not ($userPath -split ";" | Where-Object { $_ -eq $installDir })) {
		[Environment]::SetEnvironmentVariable("Path", "$userPath;$installDir", "User")
		Write-Host ""
		Write-Host "bil: added $installDir to your user PATH. Restart your terminal to pick it up."
	}

	if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
		Write-Host ""
		Write-Host "bil: note - 'go' was not found on PATH. 'bil run' shells out to the Go"
		Write-Host "bil: toolchain to execute transpiled programs (Bil compiles to Go and runs"
		Write-Host "bil: on it directly). 'bil vet' works without it. Install Go from:"
		Write-Host "bil:   https://go.dev/dl/"
	}
} finally {
	Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
