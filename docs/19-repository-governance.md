# Repository Governance

## Protected Branches

`main` is the protected release branch for Holo VTL.

Rules:

- Do not force-push to `main`.
- Do not delete `main`.
- Merge changes through pull requests.
- Require at least one approving review before merge.
- Dismiss stale approvals after new pushes.
- Resolve all review conversations before merge.

Repository administrators may retain bypass ability for emergency release operations, but normal feature, fix, and documentation changes should still use pull requests.

## Required Checks

Required status checks should be enabled only for stable, always-on CI jobs.

Good candidates:

- guardrails
- control-plane tests
- data-plane tests
- web-console tests and build
- integration contract smoke tests

Do not make privileged, manually gated, or environment-specific jobs required for every pull request. For example, `e2e-scsi-gated` depends on a privileged Linux runner and should remain an explicit release-validation gate rather than a universal merge blocker.

## Release Tags

Release tags use the `Holo@vX.Y.Z` format, matching the existing public release history.
