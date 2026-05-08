# Integration Scenario: Dedup Hit and Collision

1. Initialize fresh layout.
2. Write same payload twice with dedup enabled; verify one dedup entry with refcount=2.
3. Inject collision case (same fingerprint, different checksum/length) and verify separate entry.
