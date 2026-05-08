# Integration Scenario: Crash Recovery Failpoints

1. Trigger write failpoint after lookup append with dirty checkpoint persisted.
2. Restart recovery routine.
3. Verify checkpoint returns to clean state and logical readback remains valid.
4. Verify dedup refcount and reclaim safety reconciliation after recovery.
