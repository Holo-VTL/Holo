# Feature Specification: Web Console Dist Hardening

**Feature Branch**: `codex/048-release-followup-hardening`
**Created**: 2026-05-11
**Status**: Draft
**Input**: User description: "Continue remaining release hardening after pausing the RC."

## User Scenario

As a release maintainer, I want the built Web Console artifact to carry browser-enforced integrity and content security policy metadata, so that the packaged UI has defense-in-depth even before operator-specific reverse proxy headers are added.

## Requirements

- **FR-001**: Production `npm run build` MUST add a CSP meta tag to `dist/index.html`.
- **FR-002**: Production `npm run build` MUST add SHA-384 SRI attributes to generated JS and CSS assets referenced from `dist/index.html`.
- **FR-003**: The hardening step MUST fail the build if no generated `/ui/assets/*` entries are found.
- **FR-004**: The development source `index.html` MUST remain compatible with Vite dev mode.

## Success Criteria

- **SC-001**: `cd web-console && npm run build` succeeds and produces `integrity="sha384-..."` entries.
- **SC-002**: The generated `dist/index.html` contains a `Content-Security-Policy` meta tag.
- **SC-003**: `cd web-console && npm test` continues to pass.
