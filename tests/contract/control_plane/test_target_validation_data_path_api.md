# Contract Test: Target Validation Data Path API

## Scope
- `POST /v1/targets/publications/{publicationId}/validation-runs`
- `GET /v1/targets/publications/{publicationId}/validation-runs`

## Assertions
- fixed mode request returns `202` with `mode=fixed` and digest evidence fields
- empty mode request returns `202` with `mode=empty` and zero-byte write/read counts
- unsupported mode returns `400`
- validation list includes mode and evidence metadata for completed runs
