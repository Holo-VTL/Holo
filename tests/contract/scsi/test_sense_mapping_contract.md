# Contract Test: Identity Sense Mapping

## Purpose
Validate deterministic sense mapping for compatibility-critical identity failures.

## Checks
1. Unsupported VPD request maps to ILLEGAL REQUEST tuple.
2. Missing media maps to NOT READY tuple.
3. Access-denied context maps to ACCESS DENIED tuple.
