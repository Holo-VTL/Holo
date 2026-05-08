# Integration Test: Discovery Publish Flow

## Steps
1. Create resource chain via `POST /v1/resources/chain`.
2. Publish target via `POST /v1/targets/publications`.
3. Poll `GET /v1/targets/publications/{id}` until state `ready`.
4. Execute host discovery workflow (Linux + Windows).

## Expected
- publication reaches `ready`
- target IQN and portal are returned
- discovery evidence captured in validation artifacts
