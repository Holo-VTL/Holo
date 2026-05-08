# Integration Test: Publication Authorization Flow

## Steps
1. Create resource chain and publish target.
2. Apply rules: allow `init-a`, deny `init-b`.
3. Call authorize endpoint for `init-a`, `init-b`, and `init-c`.

## Expected
- `init-a` is allowed
- `init-b` is denied
- unmatched `init-c` is denied by default
- audit stream contains `authorize_initiator` events with decision details
