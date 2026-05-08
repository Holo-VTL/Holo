# Integration Test: Metadata Corruption Safe Fail

## Steps
1. Initialize layout and persist valid metadata.
2. Inject checksum corruption into metadata segment.
3. Reload metadata and trigger quarantine path.

## Expected
- parser detects corruption and returns error
- corrupted file is quarantined
- invalid metadata is not activated as active root
