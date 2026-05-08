# Contract Test: Target Discovery Audit & Ops

## Scope
- `GET /v1/audit/events`
- `GET /v1/health`

## Assertions
- publish/unpublish/discovery operations emit queryable audit events
- discovery events include initiator and visible count details
- health response includes `target-discovery` component summary
