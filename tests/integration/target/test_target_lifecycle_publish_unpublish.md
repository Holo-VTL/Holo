# Integration Test: Target Lifecycle Publish/Unpublish

## Steps
1. Publish target publication.
2. Publish same IQN again and verify conflict.
3. Unpublish publication and verify disabled state.
4. Unpublish same publication again.

## Expected
- first publish succeeds
- duplicate publish returns conflict
- unpublish is successful and idempotent
