# Integration Harness Setup

## Environment Baseline
- Start services: `docker compose -f infra/deploy/docker-compose.yml up -d`
- Control-plane health endpoint: `GET /healthz` returns `200 OK`.
- Data-plane process logs startup banner.

## Shared Test Inputs
- Pre-create one pool, one library, one drive, one cartridge.
- Use one authorized and one unauthorized client identity.

## Harness Conventions
- Keep integration scenarios in markdown-first form for early validation.
- Convert critical scenarios to executable scripts as implementation matures.
