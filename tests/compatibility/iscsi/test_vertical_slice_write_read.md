# Compatibility Test: Vertical Slice Minimal Write-Read

## Steps
1. Publish target and ensure `ready` state.
2. Trigger validation run for minimal write-read.
3. Query validation run result.

## Expected
- validation status is `passed`
- bytes written equals bytes read
- evidence path is persisted in result payload
