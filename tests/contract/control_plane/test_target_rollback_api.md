# Contract Test: Target Rollback API

## Scope
- `POST /v1/targets/publications/{publicationId}/rollback`

## Assertions
- rollback returns `200`
- publication becomes `disabled`
- repeated rollback is idempotent and remains `disabled`
- rollback operation is auditable through `/v1/audit/events`
