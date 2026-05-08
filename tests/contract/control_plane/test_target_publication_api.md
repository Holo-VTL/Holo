# Contract Test: Target Publication API

## Scope
- `POST /v1/resources/chain`
- `POST /v1/targets/publications`
- `GET /v1/targets/publications`
- `GET /v1/targets/publications/{publicationId}`
- `DELETE /v1/targets/publications/{publicationId}`

## Assertions
- publish returns `202` with publication payload
- duplicate active IQN returns `409`
- get/list return publication state and target identity fields
- delete transitions state to `disabled`
