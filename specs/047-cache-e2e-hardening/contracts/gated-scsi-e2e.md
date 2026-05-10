# Contract: Gated Real SCSI E2E Harness

## Command

```sh
bash scripts/gated-scsi-e2e.sh [--check-only] --portal HOST[:PORT] --iqn IQN [--write-read-smoke]
```

Release maintainers can also use the repository target:

```sh
make e2e-scsi
```

## Gating

- Default invocation without `HOLO_RUN_PRIVILEGED_SCSI_E2E=1` exits before host mutation.
- `--check-only` performs gating and prerequisite checks only, never discovery/login.
- Non-Linux hosts exit before mutation.
- Non-root users exit before mutation.

## Required Commands

- `iscsiadm`
- `sg_inq`
- `sg_readcap`
- `findmnt`
- `lsblk`

Optional write/read smoke commands:
- `sg_dd` or `dd`

## Observable Output

- Each major step prints an `[e2e]` line.
- Missing prerequisites print `[e2e][skip]`.
- Mutating operations print `[e2e][run]`.
- Cleanup prints `[e2e][cleanup]`.

## Exit Codes

- `0`: checks or E2E completed.
- `2`: gated skip or missing prerequisite before mutation.
- `1`: E2E started and failed.

## GitHub Actions Wiring

- Workflow: `.github/workflows/e2e-scsi.yml`.
- Pull request trigger: add label `run-e2e`.
- Scheduled trigger: daily at `0 6 * * *`.
- Runner contract: self-hosted labels `linux` and `privileged-scsi`.
- Secrets: `HOLO_E2E_PORTAL` and `HOLO_E2E_IQN`.

## Cleanup

The script must best-effort logout the IQN from the portal and remove temporary files on normal exit, error, or interrupt.
