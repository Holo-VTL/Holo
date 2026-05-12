# Tasks: Release Follow-Up Hardening

**Input**: `/Users/lei/AI_CC_Home/vtlix/specs/048-release-followup-hardening/spec.md`

## Phase 1: Control-Plane Review Follow-Ups

- [X] T001 Include quoted invalid config env values in integer/bool parse errors and logs.
- [X] T002 Replace `os.Args[0]` test-binary suffix check with `testing.Testing()`.
- [X] T003 Document `SafeJoin(root, "")` root-normalization behavior.
- [X] T004 Pass mapped domain errors into `respondError` so logs keep internal context.

## Phase 2: Rust Clippy Gate

- [X] T005 Run `cargo clippy --locked --all-targets -- -D warnings` and identify existing warnings.
- [X] T006 Apply mechanical Clippy cleanup without changing persisted formats or SCSI semantics.
- [X] T007 Tighten the production Rust cast scanner baseline from 248 to 247.

## Phase 3: CI And Web Console

- [X] T008 Add `go vet ./...` to MVP CI.
- [X] T009 Add `cargo clippy --locked --all-targets -- -D warnings` to MVP CI.
- [X] T010 Add web-console `npm ci`, `npm audit --audit-level=high`, `npm test`, and `npm run build` to MVP CI.
- [X] T011 Bind Vite dev and preview scripts/config to `127.0.0.1` by default.

## Phase 4: Verification

- [X] T012 Run control-plane tests and vet.
- [X] T013 Run data-plane tests and Clippy.
- [X] T014 Run guardrail, cast scan, and gated SCSI shell tests.
- [X] T015 Run web-console audit, tests, and build.
