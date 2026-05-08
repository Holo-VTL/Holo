# Contract: TCMU Publish/Unpublish Lifecycle

## Purpose
Ensure publish/unpublish sequence leaves no leaked TCMU or target objects.

## Steps
1. Publish one target in `tcmu` mode.
2. Check `targetcli ls` contains `/backstores/user:holo/<name>`.
3. Unpublish same target.
4. Re-check `targetcli ls` and socket path.

## Expected
- Publish creates iSCSI target + user backstore + socket
- Unpublish removes iSCSI target + user backstore
- Socket file `/run/holo/cdb-<publication>.sock` removed
- Repeated unpublish is idempotent
