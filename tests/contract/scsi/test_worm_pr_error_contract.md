# Contract: WORM + PR Error Stability

## Scenario
- Trigger WORM overwrite attempt.
- Trigger PR reservation conflict.
- Trigger invalid PR key operation.

## Expected
- WORM overwrite: deterministic write-protect class.
- Reservation conflict: deterministic reservation-conflict class.
- Invalid key: deterministic illegal-request class.
