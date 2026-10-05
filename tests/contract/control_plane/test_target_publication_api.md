# Contract Test: Target Publication API

## Scope
- `POST /v1/libraries`
- `POST /v1/drives`
- `POST /v1/cartridges`
- `POST /v1/targets/publications`
- `GET /v1/targets/publications`
- `GET /v1/targets/publications/{publicationId}`
- `DELETE /v1/targets/publications/{publicationId}`

## Assertions
- create fixture resources through the standard endpoints; libraries have no pool binding and each cartridge has one pool binding
- the removed demo route `/v1/resources/chain` returns `404` and cannot overwrite existing resource state
- publish returns `202` with publication payload
- duplicate active IQN returns `409`
- get/list return publication state and target identity fields
- delete transitions state to `disabled`
