# Contract: SPACE Branch Semantics

## Scenario
1. Prepare mixed blocks and filemarks.
2. Execute `SPACE blocks` with positive and negative counts.
3. Execute `SPACE filemarks` with positive and negative counts.
4. Execute `SPACE eod` baseline movement.

## Expected
- Position transitions are deterministic for all supported branches.
- Out-of-range movement is rejected with stable illegal-request class behavior.
