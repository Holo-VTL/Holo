# Contract Test: Command Error Baseline

## Purpose
Validate deterministic error handling for minimal C1/C2 chain.

## Checks
1. No-media commands map to NOT READY baseline.
2. Locate out-of-range maps to ILLEGAL REQUEST baseline.
3. Read-not-found maps to deterministic command error class.
