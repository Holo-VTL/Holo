# Integration Test: Target Discovery Flow

## Steps
1. Publish multiple targets.
2. Configure ACL policies (allow and deny).
3. Query discovery endpoint for initiator.
4. Unpublish one target and query again.

## Expected
- discovery returns only ready + allowed targets
- unpublished target disappears from discovery results
- portal filter can narrow discovery list
