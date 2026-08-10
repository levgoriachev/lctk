param(
    [Parameter(Mandatory = $true)][string]$Repository,
    [Parameter(Mandatory = $true)][string]$Output,
    [int]$Limit = 400,
    [string]$State = "merged"
)

$ErrorActionPreference = "Stop"

# Ticket titles are the only problem-statement source in these repositories that
# was written before the fix, by the person who hit the bug. A commit subject is
# written by the person who fixed it and a commit body is an engineering
# narrative that names the failing function outright, so both leak the answer
# this benchmark exists to make an agent find.
#
# The titles live in pull-request bodies as tracker links whose URL slug is the
# title. This collects them into the pinnable JSONL the corpus builder reads,
# and does nothing else: the builder itself stays hermetic, and this file
# becomes a campaign input verified by SHA-256 like every other one.

$parent = Split-Path -Parent $Output
if ($parent -and -not (Test-Path $parent)) {
    New-Item -ItemType Directory -Force $parent | Out-Null
}

Write-Host "Reading up to $Limit $State pull requests from $Repository..."
$raw = & gh pr list --repo $Repository --state $State --limit $Limit --json number,title,body
if ($LASTEXITCODE -ne 0) {
    throw "gh pr list failed for $Repository"
}

$pulls = $raw | ConvertFrom-Json
$records = New-Object System.Collections.Generic.List[string]
$seen = New-Object System.Collections.Generic.HashSet[string]
$withTicket = 0

foreach ($pull in $pulls) {
    $body = $pull.body
    if (-not $body) { continue }

    # A tracker URL ends in a human-readable slug followed by the ticket id. Both
    # halves matter: the id is what a commit subject references, and the slug is
    # the reporter's own description of the symptom.
    $matches = [regex]::Matches($body, 'https://[A-Za-z0-9.-]+/[^\s)\]]*?/([A-Za-z0-9-]+?)-(\d+)(?=[\s)\]]|$)')
    if ($matches.Count -eq 0) { continue }
    $withTicket++

    foreach ($match in $matches) {
        $slug = $match.Groups[1].Value
        $ref = $match.Groups[2].Value
        if (-not $seen.Add($ref)) { continue }

        $title = ($slug -replace '[-_]', ' ') -replace '\s+', ' '
        $title = $title.Trim()
        if ($title.Length -lt 10) { continue }

        $record = [ordered]@{
            ref          = $ref
            title        = $title
            source_pull  = $pull.number
            source_title = $pull.title
        }
        $records.Add(($record | ConvertTo-Json -Compress -Depth 3))
    }
}

# Written without a byte-order mark: this file is read by a JSON decoder, and a
# BOM makes the first record unparseable.
$resolved = [System.IO.Path]::GetFullPath((Join-Path (Get-Location) $Output))
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
[System.IO.File]::WriteAllLines($resolved, $records, $utf8NoBom)
$digest = (Get-FileHash -Path $resolved -Algorithm SHA256).Hash.ToLower()

Write-Host "pull requests read: $($pulls.Count)"
Write-Host "pull requests carrying a tracker link: $withTicket"
Write-Host "distinct ticket statements written: $($records.Count)"
Write-Host "output: $Output"
Write-Host "sha256: $digest"
