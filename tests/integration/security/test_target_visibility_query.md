# Integration Test: Target Visibility Query

## Steps
1. Publish at least two targets.
2. Configure different ACL rules per publication.
3. Query `GET /v1/targets/visible` for each initiator.

## Expected
- each initiator only sees ACL-allowed ready publications
- results are deterministic across repeated calls
- audit stream contains `query_visible_publications` events
