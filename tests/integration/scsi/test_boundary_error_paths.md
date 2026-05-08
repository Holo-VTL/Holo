# Integration: Boundary Error Paths

## Flow
1. Attempt SPACE movement before BOT and beyond EOD.
2. Query unsupported MODE/LOG pages.
3. Attempt write with fixed-size mismatch.

## Assertions
- Failure signatures are stable and repeatable.
- No silent state corruption occurs after failed operations.
