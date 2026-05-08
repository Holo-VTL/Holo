# Integration: WORM Protection Paths

## Setup
- Two command phases on the same loaded cartridge.

## Flow
1. Set WORM lock ON and attempt WRITE/ERASE.
2. Set WORM lock OFF and attempt WRITE again.

## Assertions
- Phase 1 operations are blocked with deterministic write-protect behavior.
- Phase 2 WRITE succeeds and position advances.
