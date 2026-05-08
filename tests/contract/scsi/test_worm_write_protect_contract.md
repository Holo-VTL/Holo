# Contract: WORM Write-Protect Behavior

## Scenario
1. Load cartridge.
2. Enable WORM retention lock.
3. Issue WRITE and ERASE.

## Expected
- WRITE returns write-protect class error.
- ERASE returns write-protect class error.
- No data mutation occurs while lock is active.
