# Feature Specification: Release Follow-Up Hardening

**Feature Branch**: `codex/048-release-followup-hardening`  
**Created**: 2026-05-11  
**Status**: Draft  
**Input**: User description: "Pause the v1.0.1 RC release, continue with the remaining low-risk review items, and make the release content stronger before the next RC."

## User Scenarios & Testing

### User Story 1 - Close Low-Risk Review Follow-Ups (Priority: P1)

As a release maintainer, I want the deferred Round-2 NITs closed before the next RC, so that review debt does not linger across releases.

**Independent Test**: Run control-plane tests and verify the changed error/logging/path helper behavior remains covered.

**Acceptance Scenarios**:

1. **Given** invalid numeric or boolean config env values, **When** config loading fails or falls back, **Then** the error/log includes the env name and quoted invalid value.
2. **Given** storage root policy tests run, **When** test-only temp roots are allowed, **Then** the code uses `testing.Testing()` instead of executable-name suffix checks.
3. **Given** `SafeJoin(root, "")` is used, **When** maintainers review the API, **Then** its root-normalization behavior is documented.
4. **Given** mapped API errors are returned, **When** `respondError` is called, **Then** the internal error is still available to logs.

### User Story 2 - Promote Static Analysis To CI (Priority: P1)

As a maintainer, I want low-noise static analysis to run in CI, so that regressions are caught before manual RC testing.

**Independent Test**: Run `go vet`, `cargo clippy --all-targets -- -D warnings`, and existing guardrail scripts locally.

**Acceptance Scenarios**:

1. **Given** Go code changes, **When** CI runs, **Then** `go vet ./...` runs after tests.
2. **Given** Rust code changes, **When** CI runs, **Then** `cargo clippy --locked --all-targets -- -D warnings` runs after tests.
3. **Given** the Rust cast scanner runs, **When** production casts decrease, **Then** the baseline is tightened.

### User Story 3 - Harden Web Console Development And CI Defaults (Priority: P2)

As a web-console maintainer, I want local dev servers bound to loopback and web checks in CI, so that frontend regressions and accidental network exposure are less likely.

**Independent Test**: Run web-console `npm audit`, `npm test`, and `npm run build`.

**Acceptance Scenarios**:

1. **Given** a developer starts Vite, **When** `npm run dev` or `npm run preview` is used, **Then** the server binds to `127.0.0.1` by default.
2. **Given** CI runs, **When** the web-console job executes, **Then** it runs `npm ci`, high-severity audit, tests, and production build.
3. **Given** a remote dev backend is needed, **When** the developer sets `HOLO_DEV_BACKEND`, **Then** Vite proxy uses that env value.

## Requirements

- **FR-001**: Close the four Round-2 NIT follow-ups without changing public API behavior.
- **FR-002**: Keep empty `HOLO_API_KEY` as the appliance default.
- **FR-003**: Add `go vet` and `cargo clippy -D warnings` to existing MVP CI only after local validation passes.
- **FR-004**: Reduce or preserve the production Rust cast baseline; never raise it in this feature.
- **FR-005**: Add web-console CI checks using the existing package lock and scripts.
- **FR-006**: Bind web-console dev and preview servers to loopback by default.
- **FR-007**: Do not include architecture-scale items such as observability expansion, deep SCSI E2E, dedup checkpointing, fsync/tokio changes, or storage-map cache read-only policy in this batch.

## Success Criteria

- **SC-001**: `cd control-plane && go test ./...` and `go vet ./...` pass.
- **SC-002**: `cd data-plane && cargo test --locked` and `cargo clippy --locked --all-targets -- -D warnings` pass.
- **SC-003**: `bash scripts/lint-guardrails.sh`, `bash scripts/scan-rust-casts.sh`, and gated SCSI shell tests pass.
- **SC-004**: `cd web-console && npm audit --audit-level=high`, `npm test`, and `npm run build` pass.
- **SC-005**: The branch remains based on the combined RC validation state, preserving the existing `1.0.1-rc.1` snapshot.
