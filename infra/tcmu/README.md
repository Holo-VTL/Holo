# TCMU Handler Runtime Notes

`holo` uses a **two-stage userspace chain** to expose Type-1 tape semantics through LIO.

## Runtime Chain

1. Control-plane `TcmuAdapter.Publish()` creates `/backstores/user:holo/<name>`.
2. `tcmu-runner` loads `handler_holo.so` (subtype: `holo`) and invokes `handle_cmd`.
3. `handler_holo.so` forwards CDB frames to `/run/holo/cdb-<publication>.sock`.
4. `holo-tcmu-handler` binary serves that socket and dispatches commands into data-plane tape state machine.
5. `Unpublish()` tears down target, backstore, handler process, and socket file.

## Files

- `handler_holo.c`: `tcmu-runner` plugin entry (`handler_init`, subtype `holo`)
- `build-handler-holo.sh`: compile `handler_holo.so`
- `install-handler-holo.sh`: install plugin to system directory and restart `tcmu-runner`
- `holo-tcmu-handler@.service`: template for managing per-publication socket worker

## Build & Install (Ubuntu 22.04)

```bash
cd infra/tcmu
./build-handler-holo.sh
sudo ./install-handler-holo.sh
sudo targetcli /backstores ls | grep user:holo
```

## Data-Plane Handler Binary Resolution

Default search order used by control-plane when spawning socket worker:

1. `$HOLO_TCMU_HANDLER_BIN`
2. `/usr/local/bin/holo-tcmu-handler`
3. `holo-tcmu-handler` in `PATH`
4. `tcmu_handler` in `PATH`

## Runtime Timeout Knobs

- `HOLO_TCMU_TARGETCLI_TIMEOUT_SEC` (control-plane, default `15`): timeout for each `targetcli` invocation in `tcmu` adapter.
- `HOLO_TCMU_SOCKET_TIMEOUT_SEC` (plugin, default `30`): UNIX socket read/write timeout in `handler_holo.so`. This must be configured in the `tcmu-runner` service environment, because the plugin runs inside `tcmu-runner` rather than the control-plane process.
- `HOLO_TAPE_DRIVE_PROFILE` (data-plane identity, default `ibm-ult3580-td6`): controls INQUIRY vendor/product emulation for tape drive compatibility.
- `HOLO_SCSI_DEVICE_ROLE` (per-handler, default `drive`): `drive` or `changer`.
- `HOLO_SCSI_CHANGER_PROFILE` (when role is `changer`, default `ibm-03584l32`): controls medium changer INQUIRY identity.

### Tape Identity Profiles

Supported `HOLO_TAPE_DRIVE_PROFILE` values:

- `ibm-ult3580-td4`
- `ibm-ult3580-td5`
- `ibm-ult3580-td6` (default)
- `hp-ultrium-5-scsi`
- `hp-ultrium-6-scsi`
- `holo-lto9`
