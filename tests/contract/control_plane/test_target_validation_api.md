# Contract Test: Target Validation API

## Scope
- `POST /v1/targets/publications/{publicationId}/validation-runs`
- `GET /v1/targets/publications/{publicationId}/validation-runs`

## Assertions
- starting validation on `ready` publication returns `202`
- validation payload contains scenario/status/bytes fields
- listing validation runs includes newly created run and evidence path
