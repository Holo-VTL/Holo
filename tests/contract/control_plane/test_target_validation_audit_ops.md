# Contract Test: Target Validation Audit/Ops

## Scope
- `GET /v1/audit/events`
- `GET /v1/health`

## Assertions
- validation operations emit audit events with mode and digest summary in details
- health endpoint remains healthy after fixed/empty validation runs
- runtime health counters stay consistent after repeated validation calls
