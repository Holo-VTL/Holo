# Integration Test: Host Unpublish Cleanup

## Steps
1. Publish target and complete one successful initiator login.
2. Unpublish via `DELETE /v1/targets/publications/{id}`.
3. Re-run discovery on Linux/Windows initiators.
4. Verify previous session cannot be re-established.

## Expected
- unpublished target no longer appears in discovery results
- initiator session is absent after cleanup
- repeated unpublish remains idempotent and safe
