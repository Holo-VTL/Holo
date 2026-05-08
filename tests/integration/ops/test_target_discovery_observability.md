# Integration Test: Target Discovery Observability

## Steps
1. Execute publish, unpublish, and discovery operations.
2. Query audit and health endpoints.

## Expected
- audit stream includes `discover_targets` and lifecycle actions
- health output contains discovery summary fields
