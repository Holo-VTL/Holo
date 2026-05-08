# Contract Test: Target Access Rollback API

## Scope
- `POST /v1/targets/publications/{publicationId}/access-rollback`

## Assertions
- rollback returns `200` and includes `snapshot` + `noop`
- rollback restores previous rule snapshot decisions
- rollback action is queryable from `/v1/audit/events`
