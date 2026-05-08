# Contract Test: Core Resource APIs

- [ ] POST /v1/pools returns 201 with pool_id
- [ ] GET /v1/pools returns 200 with pool list
- [ ] GET /v1/pools/{poolId} returns 200 with pool detail
- [ ] POST /v1/libraries returns 201 and references existing pool
- [ ] GET /v1/libraries returns 200 with library list
- [ ] GET /v1/libraries/{libraryId} returns 200 with library detail
- [ ] POST /v1/drives returns 201 with unique slot in library
- [ ] GET /v1/drives returns 200 with drive list
- [ ] GET /v1/drives/{driveId} returns 200 with drive detail
- [ ] POST /v1/cartridges returns 201 with lifecycle_state=available
- [ ] GET /v1/cartridges returns 200 with cartridge list
- [ ] GET /v1/cartridges/{cartridgeId} returns 200 with cartridge detail
- [ ] POST /v1/cartridges/{id}/mount returns 200 for valid drive
