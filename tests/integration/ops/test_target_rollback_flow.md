# Integration Test: Target Rollback Flow

## Steps
1. Publish target and confirm `ready` state.
2. Trigger rollback endpoint.
3. Query publication state and health endpoint.
4. Query audit events for rollback action.

## Expected
- publication state is `disabled`
- health endpoint remains healthy and includes target-runtime component
- rollback audit event includes actor/action/object/result
