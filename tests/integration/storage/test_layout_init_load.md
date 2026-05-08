# Integration Test: Layout Init and Load

## Steps
1. Create empty cartridge backend directory.
2. Run layout initialization.
3. Reload layout metadata and roots.

## Expected
- all required segment files are created
- metadata checkpoint is valid and loadable
- no fallback/default mutation during load
