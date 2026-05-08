# Contract Test: Target Visibility API

## Scope
- `GET /v1/targets/visible?initiator=...`

## Assertions
- response returns `200` and includes `initiator` echo field
- only ACL-allowed ready publications are listed
- denied and unmatched publications are excluded
- missing initiator query parameter returns `400`
