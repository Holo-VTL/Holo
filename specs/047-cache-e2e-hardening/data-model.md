# Data Model: Cache Invalidation And Gated SCSI E2E

## Cache Invalidation Event

- `layout_root`: canonical cartridge layout path affected by the cleanup attempt.
- `operation`: cache operation, initially `read_prefetch_invalidate`.
- `result`: `ok`, `failed`, or `bypassed`.
- `failure_count`: saturating in-memory counter for the layout root.
- `reason`: safe public error string for logs/tests.

Validation rules:
- A failed post-commit invalidation cannot change an already-successful write response to failure.
- Failure state is bounded by layout root and cleared when layout caches are discarded.

## Cache Degradation State

- `layout_root`: affected layout.
- `read_prefetch_bypassed`: true when prefetched reads must not be consumed or scheduled.
- `invalidation_failures`: saturating count.

State transitions:
- `healthy -> degraded`: invalidation failure is observed.
- `degraded -> healthy`: layout cache discard/unload clears in-memory prefetch state.
- `degraded -> degraded`: repeated failures increment the bounded counter and keep bypass active.

## E2E Harness Run

- `portal`: target portal host and port.
- `iqn`: Holo iSCSI target IQN.
- `device`: discovered SCSI device path after login.
- `runtime_dir`: temporary files and evidence directory.
- `commands`: ordered command evidence for prerequisite checks, discovery/login, `sg_inq`, `sg_readcap`, optional smoke, cleanup.
- `cleanup_status`: best-effort logout and temporary file cleanup result.

Validation rules:
- The harness exits before mutation unless explicit opt-in is present.
- Linux, root, and command prerequisites are checked before discovery/login.
- Cleanup trap runs on success, failure, and interruption.
