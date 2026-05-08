# Contract Test: Target Lifecycle API

## Scope
- `POST /v1/targets/publications`
- `DELETE /v1/targets/publications/{publicationId}`
- `GET /v1/targets/publications`

## Assertions
- publish returns `202` with lifecycle state transition to `ready`
- duplicate active IQN publish returns `409`
- unpublish returns `202` and publication state is `disabled`
- repeated unpublish is idempotent and remains successful
