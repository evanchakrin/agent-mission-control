param(
    [Parameter(Mandatory=$true)][string]$CandidateDir,
    [Parameter(Mandatory=$true)][string]$Executable,
    [Parameter(Mandatory=$true)][string]$ResultPath
)
$ErrorActionPreference = 'Stop'
$hub = 'C:\ProgramData\AgentMissionControl\personal-20260908-live\hub'
$previous = Join-Path $hub 'old-history-20260919'
$release = 'C:\Program Files\AgentMissionControl\releases\8.0.0-stats.1'
$newExe = Join-Path $release 'amc.exe'
$config = 'C:\ProgramData\AgentMissionControl\config\hub-personal-20260908.json'
$oldExe = 'C:\Program Files\AgentMissionControl\releases\personal-20260909-finished1\amc.exe'
$expectedHash = '522E3F385F2A65D0A7C48B2D6DA977C404AE9F5592FEFEDA0071F7FBBA4E4411'
$result = [ordered]@{ phase='preflight'; switched=$false; erpHeartbeat=$false; oldDatabaseDeleted=$false; oldBlobsDeleted=$false; problem='' }

function Save-Result {
    $json = $result | ConvertTo-Json -Depth 5
    [IO.File]::WriteAllText($ResultPath, $json + "`n")
}
function Set-ServiceImage([string]$exe) {
    $image = '"' + $exe + '" hub --config ' + $config
    $service = Get-CimInstance Win32_Service -Filter "Name='AMCHub'" -ErrorAction Stop
    $changed = Invoke-CimMethod -InputObject $service -MethodName Change -Arguments @{PathName=$image} -ErrorAction Stop
    if ($changed.ReturnValue -ne 0) { throw "Could not set AMCHub image path to $exe (Windows code $($changed.ReturnValue))" }
    $updated = Get-CimInstance Win32_Service -Filter "Name='AMCHub'" -ErrorAction Stop
    if ($updated.PathName -ne $image) { throw "AMCHub image path did not match $exe after update" }
}
function Wait-Hub {
    (Get-Service AMCHub).WaitForStatus('Running', [TimeSpan]::FromSeconds(60))
    $deadline = (Get-Date).AddSeconds(90)
    do {
        try {
            $totals = Invoke-RestMethod 'http://127.0.0.1:4174/api/v2/totals' -TimeoutSec 8
            if ($totals.sessions -gt 0) { return }
        } catch { }
        Start-Sleep -Seconds 3
    } while ((Get-Date) -lt $deadline)
    throw 'New hub did not serve session totals'
}

try {
    $candidate = [IO.Path]::GetFullPath($CandidateDir)
    $binary = [IO.Path]::GetFullPath($Executable)
    $resultFile = [IO.Path]::GetFullPath($ResultPath)
    if ($resultFile -ne $ResultPath) { throw 'Result path must be absolute' }
    if (!(Test-Path -LiteralPath (Join-Path $candidate 'ledger.sqlite'))) { throw 'Compact ledger missing' }
    if (!(Test-Path -LiteralPath (Join-Path $candidate 'blobs'))) { throw 'Pending-chunk directory missing' }
    if ((Get-Item -LiteralPath (Join-Path $candidate 'ledger.sqlite')).Length -lt 1048576) { throw 'Compact ledger is unexpectedly small' }
    if ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash -ne $expectedHash) { throw 'Release executable hash mismatch' }
    if ((Get-Service AMCHub).Status -ne 'Running') { throw 'Old hub is not running' }
    if (Test-Path -LiteralPath $previous) {
        if (@(Get-ChildItem -LiteralPath $previous -Force).Count -ne 0) { throw 'Old-history destination is not empty' }
        Remove-Item -LiteralPath $previous -Force -ErrorAction Stop
    }
    if ((Test-Path -LiteralPath $release) -and ((Get-FileHash -LiteralPath $newExe -Algorithm SHA256).Hash -ne $expectedHash)) { throw 'Staged release hash mismatch' }
    if (!(Test-Path -LiteralPath $oldExe)) { throw 'Old release missing' }
    if (!(Test-Path -LiteralPath (Join-Path $hub 'ledger.sqlite'))) { throw 'Old ledger missing' }
    $result.phase = 'stopping-old-hub'; Save-Result
    Stop-Service AMCHub -ErrorAction Stop
    (Get-Service AMCHub).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
    $result.phase = 'installing-compact-ledger'; Save-Result
    New-Item -ItemType Directory -Path $previous -ErrorAction Stop | Out-Null
    foreach ($name in @('ledger.sqlite','ledger.sqlite-wal','ledger.sqlite-shm')) {
        $path = Join-Path $hub $name
        if (Test-Path -LiteralPath $path) { Move-Item -LiteralPath $path -Destination (Join-Path $previous $name) -ErrorAction Stop }
    }
    Move-Item -LiteralPath (Join-Path $hub 'blobs') -Destination (Join-Path $previous 'blobs') -ErrorAction Stop
    Copy-Item -LiteralPath (Join-Path $candidate 'ledger.sqlite') -Destination (Join-Path $hub 'ledger.sqlite') -ErrorAction Stop
    $candidateWal = Join-Path $candidate 'ledger.sqlite-wal'
    if (Test-Path -LiteralPath $candidateWal) { Copy-Item -LiteralPath $candidateWal -Destination (Join-Path $hub 'ledger.sqlite-wal') -ErrorAction Stop }
    Copy-Item -LiteralPath (Join-Path $candidate 'blobs') -Destination (Join-Path $hub 'blobs') -Recurse -ErrorAction Stop
    if (!(Test-Path -LiteralPath $release)) {
        New-Item -ItemType Directory -Path $release -ErrorAction Stop | Out-Null
        Copy-Item -LiteralPath $binary -Destination $newExe -ErrorAction Stop
    }
    if ((Get-FileHash -LiteralPath $newExe -Algorithm SHA256).Hash -ne $expectedHash) { throw 'Installed executable hash mismatch' }
    Set-ServiceImage $newExe
    $result.phase = 'starting-new-hub'; Save-Result
    Start-Service AMCHub -ErrorAction Stop
    Wait-Hub
    $result.switched = $true
    $result.phase = 'awaiting-erp-heartbeat'; Save-Result
    $deadline = (Get-Date).AddSeconds(120)
    $cutoverAt = (Get-Date).ToUniversalTime().AddSeconds(-5)
    do {
        try {
            $machines = Invoke-RestMethod 'http://127.0.0.1:4174/api/v2/machines' -TimeoutSec 8
            $erp = $machines | Where-Object { $_.machine.heartbeat.machineId -eq 'trifecta-erp' } | Select-Object -First 1
            if ($erp -and $erp.connection -eq 'connected' -and [DateTimeOffset]::Parse($erp.machine.lastSeen).UtcDateTime -ge $cutoverAt) {
                $result.erpHeartbeat = $true
                break
            }
        } catch { }
        Start-Sleep -Seconds 5
    } while ((Get-Date) -lt $deadline)
    if (!$result.erpHeartbeat) { throw 'ERP did not check in on the new hub; old history preserved' }
    $result.phase = 'deleting-old-storage'; Save-Result
    # All destructive targets are fixed children of the verified old-history
    # directory inside the explicit AMC hub data directory.
    if ([IO.Path]::GetFullPath($previous) -ne 'C:\ProgramData\AgentMissionControl\personal-20260908-live\hub\old-history-20260919') { throw 'Unexpected deletion target' }
    foreach ($name in @('ledger.sqlite','ledger.sqlite-wal','ledger.sqlite-shm')) {
        $path = Join-Path $previous $name
        if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force -ErrorAction Stop }
    }
    $result.oldDatabaseDeleted = $true; Save-Result
    Remove-Item -LiteralPath (Join-Path $previous 'blobs') -Recurse -Force -ErrorAction Stop
    $result.oldBlobsDeleted = $true
    Remove-Item -LiteralPath $previous -Force -ErrorAction Stop
    $result.phase = 'complete'; Save-Result
} catch {
    $result.problem = $_.Exception.Message
    if (!$result.switched -and (Test-Path -LiteralPath $previous)) {
        $result.phase = 'rolling-back'
        try {
            Stop-Service AMCHub -ErrorAction SilentlyContinue
            (Get-Service AMCHub).WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
            foreach ($name in @('ledger.sqlite','ledger.sqlite-wal','ledger.sqlite-shm')) {
                $live = Join-Path $hub $name
                $saved = Join-Path $previous $name
                if (Test-Path -LiteralPath $live) { Remove-Item -LiteralPath $live -Force }
                if (Test-Path -LiteralPath $saved) { Move-Item -LiteralPath $saved -Destination $live }
            }
            $liveBlobs = Join-Path $hub 'blobs'
            if (Test-Path -LiteralPath $liveBlobs) { Remove-Item -LiteralPath $liveBlobs -Recurse -Force }
            $savedBlobs = Join-Path $previous 'blobs'
            if (Test-Path -LiteralPath $savedBlobs) { Move-Item -LiteralPath $savedBlobs -Destination $liveBlobs }
            Set-ServiceImage $oldExe
            Start-Service AMCHub
            $result.phase = 'rolled-back'
        } catch {
            $result.problem += '; rollback failed: ' + $_.Exception.Message
            $result.phase = 'rollback-needs-attention'
        }
    }
    Save-Result
    exit 1
}
