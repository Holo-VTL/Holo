# Contract: Persistent Reservation Lifecycle

## Scenario
1. Register keys for initiator A and B.
2. Reserve by A.
3. B attempts protected write.
4. A releases reservation.

## Expected
- Reserve succeeds for registered owner.
- B protected write conflicts while reservation is active.
- Protected commands from B are allowed after owner release.
