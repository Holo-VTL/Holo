# Contract: Boundary Error Stability

## Scenario
- Trigger SPACE out-of-range movement.
- Trigger unsupported MODE/LOG pages.
- Trigger fixed-block mismatch in fixed mode.

## Expected
- All boundary and unsupported cases map to stable error/sense classes.
