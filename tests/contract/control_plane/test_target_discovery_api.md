# Contract Test: Target Discovery API

## Scope
- `GET /v1/targets/discovery?initiator=...`

## Assertions
- missing initiator returns `400`
- response returns `200` with `targets[]`
- only `ready` and ACL-allowed publications are returned
- optional `portal` filter narrows result set deterministically
