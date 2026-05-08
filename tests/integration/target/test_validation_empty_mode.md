# Integration Test: Validation Empty Mode

## Steps
1. Publish ready target publication.
2. Start validation run with `mode=empty`.
3. Query validation run list.

## Expected
- validation status is `passed`
- write/read bytes are zero and equal
- evidence still contains explicit digest parity metadata
