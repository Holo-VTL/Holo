# Integration: PR Owner Conflict Paths

## Setup
- Two initiator identities sharing one loaded media instance.

## Flow
1. Register A/B and reserve by A.
2. Execute write from B (expect conflict).
3. Release by A.
4. Execute write from B (expect success).

## Assertions
- Conflict and post-release success are deterministic.
