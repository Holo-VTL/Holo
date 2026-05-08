# Integration Test: Target Access Rollback

## Steps
1. Publish target and apply baseline allow policy.
2. Apply second policy that changes decision outcome.
3. Execute access rollback endpoint.
4. Re-run authorization checks and inspect audit events.

## Expected
- decision behavior returns to previous snapshot
- rollback response reports snapshot metadata and `noop=false`
- `/v1/audit/events` includes `rollback_access_rules`
