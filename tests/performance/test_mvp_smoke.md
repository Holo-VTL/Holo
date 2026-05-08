# Performance Smoke Test (MVP)

## Target
- Complete baseline backup + restore cycle within 30 minutes.

## Smoke Steps
1. Provision baseline resources.
2. Execute one backup job with fixed dataset.
3. Execute one restore job.
4. Capture duration and major error counters.

## Expected
- End-to-end duration <= 30 minutes.
- No critical command/lifecycle errors.
