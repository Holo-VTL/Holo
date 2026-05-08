# Contract Test: Read/Write/Locate/Position Chain

## Purpose
Validate minimal C1 command-chain contract.

## Checks
1. WRITE persists payload and advances logical position.
2. WRITE FILEMARKS records deterministic boundaries.
3. LOCATE repositions within valid media range.
4. READ returns payload parity at located positions.
5. READ POSITION exposes deterministic tuple fields.
