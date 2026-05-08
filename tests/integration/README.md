# Integration Contract Smoke

This directory contains reproducible cross-layer smoke checks that validate critical contracts introduced by the GLM remediation stream.

## Run locally

```bash
cd /Users/lei/AI_CC_Home/holo
bash tests/integration/contract_smoke.sh
```

## Scope (current)

- Control-plane contract smoke:
  - API auth guard remains enforced for management routes.
  - Safe request-body handling for policy endpoints.
- Data-plane contract smoke:
  - PR OUT dispatch mutates reservation state.

## Notes

- This is the initial automation baseline and is intentionally deterministic and fast.
- Additional host/network end-to-end scenarios can be layered on top in future iterations.
