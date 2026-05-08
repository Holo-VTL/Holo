# Integration: SPACE Boundary Paths

## Flow
1. Build block/filemark sequence on loaded media.
2. Traverse with SPACE blocks/filemarks/eod.
3. Trigger underflow/overflow paths.

## Assertions
- Successful transitions land on expected positions.
- BOT/EOD boundary crossings fail deterministically.
- Early warning state becomes active near EOD.
