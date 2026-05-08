# Contract Test: Target Authorize API

## Scope
- `POST /v1/targets/publications/{publicationId}/access-rules`
- `POST /v1/targets/publications/{publicationId}/authorize`

## Assertions
- replacing rules returns `200` and includes snapshot metadata
- matching allow rule returns `decision=allow`
- matching deny rule returns `decision=deny`
- unmatched initiator returns `decision=deny` with explicit default-deny reason
- malformed authorize payload returns `400`
