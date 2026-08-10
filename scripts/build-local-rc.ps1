param(
    [string]$Version = "0.1.12",
    [string]$TemplateVersion = "0.1.1",
    [string]$TemplateKeyID = "lctk-release-v1",
    [string]$TemplatePublicKey = "rSVhZIN82jXEG04WGzc9lAu5bszLjs//cSEO0bmJY8I="
)

$ErrorActionPreference = "Stop"

# The local RC is deliberately assembled only beneath the repository's ignored
# artifact directory. The guard keeps cleanup from ever following a changed
# working directory into an unrelated path.
$repository = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$dirty = @(git -C $repository status --porcelain)
if ($LASTEXITCODE -ne 0 -or $dirty.Count -ne 0) {
    throw "Local RC construction requires a clean committed worktree."
}
$artifactRoot = Join-Path $repository ".artifacts\local-rc"
$expectedPrefix = (Join-Path $repository ".artifacts") + [IO.Path]::DirectorySeparatorChar
$resolvedArtifactRoot = [IO.Path]::GetFullPath($artifactRoot)
if (-not $resolvedArtifactRoot.StartsWith($expectedPrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw "Local RC artifact directory escaped the repository."
}
if (Test-Path -LiteralPath $resolvedArtifactRoot) {
    Remove-Item -LiteralPath $resolvedArtifactRoot -Recurse -Force
}
$package = Join-Path $resolvedArtifactRoot "package"
New-Item -ItemType Directory -Force -Path $package | Out-Null

$seed = New-Object byte[] 32
$random = [Security.Cryptography.RandomNumberGenerator]::Create()
try {
    $random.GetBytes($seed)
}
finally {
    $random.Dispose()
}
$env:LCTK_RELEASE_ED25519_PRIVATE_KEY = [Convert]::ToBase64String($seed)
try {
    $publicKey = (& go run ./cmd/release-manifest --print-public-key).Trim()
    if (-not $publicKey) {
        throw "The local release public key was not generated."
    }
    $commit = (& git rev-parse HEAD).Trim()
    $publishedAt = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

    # A local RC must carry the code-intel source from the same commit as its
    # Windows binaries. The signed archive hash authenticates transport while
    # the signed OCI manifest digest authenticates the runnable image.
    $locations = Get-ItemProperty -LiteralPath "HKCU:\Software\LCTK" -ErrorAction Stop
    if (-not [IO.Path]::IsPathRooted($locations.RuntimeDataDir) -or -not [IO.Path]::IsPathRooted($locations.InstallDir)) {
        throw "The installed LCTK locations are incomplete."
    }
    $podman = Join-Path $locations.InstallDir "runtime\podman\5.8.2\bin\podman.exe"
    if (-not (Test-Path -LiteralPath $podman -PathType Leaf)) {
        throw "The private LCTK Podman client is not installed."
    }
    $previousDataHome = $env:XDG_DATA_HOME
    $previousConfigHome = $env:XDG_CONFIG_HOME
    try {
        $env:XDG_DATA_HOME = $locations.RuntimeDataDir
        $env:XDG_CONFIG_HOME = Join-Path $locations.InstallDir "runtime\podman\config"
        $imageTag = "localhost/lctk/code-intel:$Version"
        & $podman --connection lctk-runtime-root build --format docker --tag $imageTag images/code-intel
        if ($LASTEXITCODE -ne 0) { throw "Building the local code-intel image failed." }
        $builtDigest = (& $podman --connection lctk-runtime-root image inspect $imageTag --format '{{.Digest}}').Trim()
        if ($LASTEXITCODE -ne 0 -or $builtDigest -notmatch '^sha256:[0-9a-f]{64}$') {
            throw "The local code-intel image has no immutable manifest digest."
        }
        $imageArchive = Join-Path $package "lctk-code-intel.tar"
        & $podman --connection lctk-runtime-root save --format docker-archive --output $imageArchive $imageTag
        if ($LASTEXITCODE -ne 0) { throw "Saving the local code-intel image failed." }

        # Docker archive import normalizes the manifest and can therefore have
        # a different digest from the in-memory pre-save image. Stream the exact
        # candidate bytes through the same remote Podman boundary used by setup,
        # then sign the deterministic post-load digest that projects will run.
        $loadStart = New-Object Diagnostics.ProcessStartInfo
        $loadStart.FileName = $podman
        $loadStart.Arguments = "--connection lctk-runtime-root load"
        $loadStart.UseShellExecute = $false
        $loadStart.CreateNoWindow = $true
        $loadStart.RedirectStandardInput = $true
        $loadStart.RedirectStandardOutput = $true
        $loadStart.RedirectStandardError = $true
        $loadStart.EnvironmentVariables["XDG_DATA_HOME"] = $locations.RuntimeDataDir
        $loadStart.EnvironmentVariables["XDG_CONFIG_HOME"] = Join-Path $locations.InstallDir "runtime\podman\config"
        $loadProcess = New-Object Diagnostics.Process
        $loadProcess.StartInfo = $loadStart
        $loadStarted = $false
        try {
            $loadStarted = $loadProcess.Start()
            if (-not $loadStarted) { throw "Loading the local code-intel archive did not start." }
            $archiveInput = [IO.File]::OpenRead($imageArchive)
            try {
                $archiveInput.CopyTo($loadProcess.StandardInput.BaseStream)
                $loadProcess.StandardInput.Close()
            }
            finally {
                $archiveInput.Dispose()
            }
            $loadOutput = $loadProcess.StandardOutput.ReadToEnd()
            $loadError = $loadProcess.StandardError.ReadToEnd()
            $loadProcess.WaitForExit()
            if ($loadProcess.ExitCode -ne 0) {
                throw "Loading the local code-intel archive failed: $loadError"
            }
        }
        finally {
            if ($loadStarted -and -not $loadProcess.HasExited) {
                $loadProcess.Kill()
                $loadProcess.WaitForExit()
            }
            $loadProcess.Dispose()
        }
        $imageDigest = (& $podman --connection lctk-runtime-root image inspect $imageTag --format '{{.Digest}}').Trim()
        if ($LASTEXITCODE -ne 0 -or $imageDigest -notmatch '^sha256:[0-9a-f]{64}$') {
            throw "The loaded local code-intel image has no immutable manifest digest."
        }
    }
    finally {
        $env:XDG_DATA_HOME = $previousDataHome
        $env:XDG_CONFIG_HOME = $previousConfigHome
    }
    $imageReference = "localhost/lctk/code-intel@$imageDigest"
    $imageBytes = (Get-Item -LiteralPath $imageArchive).Length
    $template = Join-Path $resolvedArtifactRoot "template-manifest.json"
    $templateURL = "https://github.com/lev-goryachev/lctk/releases/download/v$TemplateVersion/release-manifest.json"
    Invoke-WebRequest -UseBasicParsing -Uri $templateURL -OutFile $template

    # The local candidate carries the same signed, pinned CDI package as an
    # official release so GPU acceptance has no ambient repository dependency.
    $nvidiaPackage = Join-Path $package "nvidia-container-toolkit-base-1.19.1-1.x86_64.rpm"
    Invoke-WebRequest -UseBasicParsing `
        -Uri "https://nvidia.github.io/libnvidia-container/stable/rpm/x86_64/nvidia-container-toolkit-base-1.19.1-1.x86_64.rpm" `
        -OutFile $nvidiaPackage
    if ((Get-Item -LiteralPath $nvidiaPackage).Length -ne 6190068 -or
        (Get-FileHash -Algorithm SHA256 $nvidiaPackage).Hash.ToLowerInvariant() -ne "b12de77bdffd3df13cea4589a1b04a133b1ffcb250b860f7349420eed37aeb5d") {
        throw "Pinned NVIDIA Container Toolkit package identity differs."
    }

    $trust = "-s -w -X github.com/lev-goryachev/lctk/internal/buildinfo.Version=$Version -X github.com/lev-goryachev/lctk/internal/buildinfo.Commit=$commit -X github.com/lev-goryachev/lctk/internal/buildinfo.Date=$publishedAt -X github.com/lev-goryachev/lctk/internal/releasebundle.TrustedKeyID=lctk-local-rc -X github.com/lev-goryachev/lctk/internal/releasebundle.TrustedPublicKey=$publicKey"
    go build -trimpath -ldflags $trust -o (Join-Path $package "lctk-core.exe") ./cmd/lctk
    go build -trimpath -ldflags "$trust -H=windowsgui" -o (Join-Path $package "lctk-setup.exe") ./cmd/lctk-setup

    $core = Join-Path $package "lctk-core.exe"
    $coreHash = (Get-FileHash -Algorithm SHA256 $core).Hash.ToLowerInvariant()
    $coreBytes = (Get-Item -LiteralPath $core).Length
    go build -trimpath -ldflags "-s -w -X main.PackagedCoreSHA256=$coreHash -X main.PackagedCoreBytes=$coreBytes" -o (Join-Path $package "lctk.exe") ./cmd/lctk-launcher

    $manifest = Join-Path $package "release-manifest.json"
    go run ./cmd/release-manifest `
        --version $Version --commit $commit --published-at $publishedAt `
        --base-url "http://127.0.0.1:4466" --key-id "lctk-local-rc" `
        --template-envelope $template --template-key-id $TemplateKeyID --template-public-key $TemplatePublicKey `
        --code-image $imageReference --code-image-bytes $imageBytes `
        --nvidia-inference-image-bytes 2586107421 --nvidia-inference-image-unpacked-bytes 4360073216 `
        --artifact "lctk-core.exe,host-core,windows,amd64,$core" `
        --artifact "lctk.exe,host-launcher,windows,amd64,$package\lctk.exe" `
        --artifact "lctk-setup.exe,installer,windows,amd64,$package\lctk-setup.exe" `
        --artifact "lctk-code-intel.tar,code-image-archive,linux,amd64,$imageArchive" `
        --artifact "nvidia-container-toolkit-base-1.19.1-1.x86_64.rpm,nvidia-container-toolkit-base,linux,amd64,$nvidiaPackage" `
        --output $manifest

    $payload = Join-Path $resolvedArtifactRoot "payload.zip"
    Compress-Archive -LiteralPath $core,(Join-Path $package "lctk.exe"),(Join-Path $package "lctk-setup.exe"),$imageArchive,$nvidiaPackage,$manifest -DestinationPath $payload
    $outputs = @()
    foreach ($candidate in @(
        @{ Name = "LCTK-Setup-local-RC.exe"; Bootstrap = "bootstrap-setup.exe"; LinkerFlags = "-s -w -H=windowsgui" },
        @{ Name = "LCTK-Uninstall-local-RC.exe"; Bootstrap = "bootstrap-uninstall.exe"; LinkerFlags = "-s -w -H=windowsgui -X main.DefaultSetupMode=uninstall" }
    )) {
        $bootstrap = Join-Path $resolvedArtifactRoot $candidate.Bootstrap
        go build -trimpath -ldflags $candidate.LinkerFlags -o $bootstrap ./cmd/lctk-local-setup
        $output = Join-Path $repository (".artifacts\" + $candidate.Name)
        $stream = [IO.File]::Open($output, [IO.FileMode]::Create, [IO.FileAccess]::Write, [IO.FileShare]::None)
        try {
            foreach ($source in @($bootstrap, $payload)) {
                $input = [IO.File]::OpenRead($source)
                try {
                    $input.CopyTo($stream)
                }
                finally {
                    $input.Dispose()
                }
            }
            $stream.Flush($true)
        }
        finally {
            $stream.Dispose()
        }
        $outputs += Get-Item -LiteralPath $output
    }
    $outputs | Select-Object FullName,Length,@{Name="SHA256";Expression={(Get-FileHash -Algorithm SHA256 $_.FullName).Hash}}
}
finally {
    Remove-Item Env:LCTK_RELEASE_ED25519_PRIVATE_KEY -ErrorAction SilentlyContinue
}
