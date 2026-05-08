# 2026-05-03 Fio Storage Performance Model

Target host: `10.10.1.184`

Scope:
- SSD path: temporary XFS mount of `/dev/sdb` at `/mnt/holo-perf-ssd/fio`
- HDD/Holo pool path: `/var/lib/holo/storage-pools/pool1/fio` inside the `holo-control-plane` mount namespace, backed by `/dev/sdc`
- Fio: `fio-3.28`
- Per main case: `runtime=30s`, `size=4G`, `direct=1`, `ioengine=libaio`

Main results:

| target | test | read MiB/s | write MiB/s | read IOPS | write IOPS | p99 read ms | p99 write ms |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| ssd | seqwrite-1m | 0.00 | 1345.75 | 0.00 | 1345.75 | 0.000 | 47.448 |
| ssd | randwrite-64k | 0.00 | 623.49 | 0.00 | 9975.79 | 0.000 | 25.297 |
| ssd | randread-64k | 264.12 | 0.00 | 4225.87 | 0.00 | 4.112 | 0.000 |
| ssd | syncwrite-1m | 0.00 | 330.51 | 0.00 | 330.51 | 0.000 | 5.079 |
| hdd | seqwrite-1m | 0.00 | 52.74 | 0.00 | 52.74 | 0.000 | 7683.965 |
| hdd | randwrite-64k | 0.00 | 68.95 | 0.00 | 1103.21 | 0.000 | 200.278 |
| hdd | randread-64k | 18.29 | 0.00 | 292.57 | 0.00 | 200.278 | 0.000 |
| hdd | syncwrite-1m | 0.00 | 52.72 | 0.00 | 52.72 | 0.000 | 95.945 |

HDD sequential write diagnostics:

| test | write MiB/s | write IOPS | p99 write ms |
| --- | ---: | ---: | ---: |
| hdd-seq-qd1-nofsync | 92.71 | 92.71 | 34.341 |
| hdd-seq-qd1-fsynclose | 75.88 | 75.88 | 79.167 |
| hdd-seq-qd4-fsynclose | 97.85 | 97.85 | 139.461 |
| hdd-seq-qd16-fsynclose-repeat | 91.24 | 91.24 | 633.340 |

Conclusion:
- SSD throughput and latency are healthy for the current safety settings.
- HDD/Holo pool throughput is in the expected virtual HDD range. The first main `hdd seqwrite-1m` run showed a large p99 tail-latency spike at high queue depth; repeat diagnostics show the spike is not stable and qd1/fsync-on-close remains around 75.88 MiB/s with p99 around 79 ms.
- No immediate storage-code rollback or safety relaxation is recommended from this fio pass.
- For HDD-heavy deployments, prefer sequential/synchronous write paths with modest queue depth. High queue-depth writes can produce large tail-latency spikes on this PVE virtual HDD.

Remote cleanup:
- Removed temporary fio files from SSD temp mount and Holo pool.
- Unmounted and removed `/mnt/holo-perf-ssd`.
