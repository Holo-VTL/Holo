# Contract: TCMU CDB Loopback

## Purpose
Verify a CDB sent through TCMU userspace path is dispatched to data-plane and returns a valid SCSI response.

## Preconditions
- Runtime mode: `tcmu`
- Handler binary resolvable
- Socket path `/run/holo/cdb-<publication>.sock` is created after publish

## Steps
1. Publish one target (`POST /v1/targets/publications`).
2. Confirm socket file exists.
3. Send INQUIRY CDB frame over socket (`0x12` standard inquiry).
4. Decode response frame.

## Expected
- `scsi_status == 0x00` (GOOD)
- Response payload includes vendor/product bytes
- No handler hang or kernel-side timeout
