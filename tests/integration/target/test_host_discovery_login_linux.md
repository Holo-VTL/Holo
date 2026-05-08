# Integration Test: Host Discovery Login Linux

## Steps
1. Start control-plane with `HOLO_TARGET_RUNTIME_MODE=lio-shell`.
2. Run `scripts/validate-host-iscsi-flow.sh` on Ubuntu 22.04 host.
3. Capture discovery, login, and `/dev/disk/by-path` evidence.

## Expected
- discovery output contains published IQN
- login establishes active iSCSI session
- host sees `iscsi-<iqn>-lun-0` device path
