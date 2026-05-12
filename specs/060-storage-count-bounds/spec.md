# Feature Specification: Storage Count Bounds

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue §2.x/§3.x minor defects, including explicit bounded integer parsing."

## User Scenario

As a maintainer loading persisted cartridge metadata, I want count fields to be validated before converting them to allocation sizes, so corrupt payloads fail clearly instead of relying on unchecked casts or oversized allocation attempts.

## Requirements

- **FR-001**: Persisted u64/u32 count fields used for vector allocation MUST be converted to `usize` through an explicit checked helper.
- **FR-002**: Legacy storage payload decoders MUST validate that the payload contains enough records for the declared count before allocating a count-sized vector.
- **FR-003**: Segment index length computation MUST use checked arithmetic before comparing encoded descriptor lengths.
- **FR-004**: Existing on-disk formats MUST remain unchanged.

## Success Criteria

- **SC-001**: Legacy blk map, dedup, lookup, reclaim, segment index, and runtime filemark decoders reject corrupt count/length combinations with `StorageError::Corrupt`.
- **SC-002**: Rust storage tests pass after the bounded count cleanup.
- **SC-003**: Rust cast scanner production baseline remains non-increasing.
