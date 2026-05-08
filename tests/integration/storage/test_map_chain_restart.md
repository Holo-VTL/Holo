# Integration Test: Map Chain Restart Consistency

## Steps
1. Initialize layout and append multiple blk_map records.
2. Build lookup index and resolve logical addresses.
3. Simulate restart and reload layout/map chain.
4. Resolve same logical addresses again.

## Expected
- locate results remain identical before and after restart
- metadata roots remain valid
