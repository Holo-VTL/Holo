# Integration Test: Validation Fixed Mode

## Steps
1. Publish ready target publication.
2. Start validation run with `mode=fixed` (optional bytes/pattern).
3. Query validation run list.

## Expected
- validation status is `passed`
- write/read bytes are non-zero and equal
- write/read digest metadata is present and consistent
