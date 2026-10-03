# VTL throughput validation — 2026-10-01

## Setup

- Holo dev server: `holovtl-rocky9-dev` (`10.10.1.193`); VTL was tested both with a local loopback initiator and the requested remote initiator `.184` (`10.10.1.184`).
- Current-branch Rust handler deployed; SHA-256: `7aa7197a0b51c27ea46bbff1d58e25b185e4b526c382543d096b4d662f6b8568`.
- VTL pool: `/dev/sdc`, 200 GiB QEMU disk, XFS; native file baseline and VTL data used the same pool.
- Tape device: `/dev/nst0`, IBM ULT3580-TD6 emulation; fixed 8 MiB blocks for throughput runs.
- Local/loopback FIO 3.35 and remote-initiator FIO 3.28; non-zero buffers (`refill_buffers=1`, `scramble_buffers=1`, `randrepeat=0`), 8 MiB sequential I/O, queue depth 1. Native: direct `libaio` plus close/end fsync. VTL: synchronous tape I/O; OS cache dropped before each read.
- Effective handler settings observed from `/proc`: compression off, dedup off, payload checksum on, storage sync every 4096 writes, read prefetch on at depth 2.

## Same-host loopback results

| Size (GiB) | Native write (MiB/s) | Loopback VTL write (MiB/s) | Write retained | Native read (MiB/s) | Loopback VTL cold read (MiB/s) | Read retained |
|---:|---:|---:|---:|---:|---:|---:|
| 4 | 106.02 | 118.23 | 111.5% | 109.02 | 109.36 | 100.3% |
| 5 | 106.90 | 114.74 | 107.3% | 109.80 | 110.76 | 100.9% |
| 6 | 106.65 | 112.52 | 105.5% | 108.65 | 110.25 | 101.5% |

| Aggregate | Native avg | VTL avg | Retained | Max deviation from median |
|---|---:|---:|---:|---:|
| Write | 106.52 | 115.17 | 108.1% | 3.04% (VTL) |
| Read | 109.15 | 110.12 | 100.9% | 0.81% (VTL) |

**Result:** both directions meet the 85% retained-throughput target and the 10% repeatability threshold. All six runs per path transferred the requested bytes with fio error count 0.

## Read/write correctness checks

- A 128 MiB fixed-block tape write/read round trip matched SHA-256: `bbac37bdc0bac28fb4817f21b9d73faefad7383cb61acea2815f12a55f1ed1fa`.
- A 64 MiB variable-block (256 KiB records) tape write/read round trip matched SHA-256: `c700a5fff2e8f9f08e5cb98dad08e6c2ce1c546cb3133a9d1f7cc8e71e00d01f`.
- Filemark forward/backward spacing commands succeeded; `mt status` showed zero soft errors.

## Remote initiator validation

- Initiator: `.184` (`lei@10.10.1.184`); Holo target: `.193` (`10.10.1.193:3260`).
- The tape target was logged in through the network, with the same 8 MiB block size and FIO non-zero-buffer settings. `.193` page cache was dropped before each read.
- The native file baseline above is on the same `.193:/dev/sdc` XFS pool used by the remote VTL writes.

| Size (GiB) | Native write (MiB/s) | Remote VTL write | Write retained | Native read (MiB/s) | Remote VTL cold read | Read retained |
|---:|---:|---:|---:|---:|---:|---:|
| 4 | 106.02 | 109.38 | 103.2% | 109.02 | 107.38 | 98.5% |
| 5 | 106.90 | 111.86 | 104.6% | 109.80 | 110.07 | 100.2% |
| 6 | 106.65 | 111.54 | 104.6% | 108.65 | 110.33 | 101.5% |

- Remote write: native avg 106.52 MiB/s, VTL avg 110.93 MiB/s, retained 104.14%, max deviation from median 1.94%.

- Remote read: native avg 109.15 MiB/s, VTL avg 109.26 MiB/s, retained 100.10%, max deviation from median 2.44%.

**Result:** both remote initiator directions meet the 85% retained-throughput target and the 10% repeatability limit. All requested bytes transferred with fio error count 0.

Remote-path read/write integrity checks:

- Fixed 8 MiB mode, 128 MiB: SHA-256 `66794d0dd019d066e803a1936815bd4b4b25b52417d036559b9f3f4342085c27` matched.
- Variable 256 KiB mode, 64 MiB: SHA-256 `df629808534517c02338bf503b3c579df2437f445aeefb8bfee2625bf6b2c237` matched.
- Filemark forward/backward spacing succeeded; tape soft error count remained 0.

## Limits

- Native and VTL flush semantics differ, and the measured ceiling is the virtualized backing disk/network in this test environment. The retention result is specific to this setup, not a hardware-independent speedup claim.
- `/dev/sdb1` on `.184` was not overwritten; the native baseline used the `.193` pool disk that also backed the VTL data.
- Handler SHA and all eighteen native, loopback, and remote fio JSON result files are preserved alongside this report.
