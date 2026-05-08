# Integration: WORM + PR Error Paths

## Flow
1. Load media and lock WORM, then issue WRITE.
2. Configure PR owner A, issue protected command from B.
3. Attempt reserve/release with mismatched key.

## Assertions
- Error behavior is stable and repeatable across runs.
- Sense/status outputs remain deterministic by error class.
