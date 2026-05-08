# Contract: MODE/LOG SENSE Key Pages

## Scenario
1. Query supported MODE page list and key pages.
2. Query supported LOG page list and key pages.
3. Query unsupported MODE/LOG page codes.

## Expected
- Key pages return stable payload schemas.
- Unsupported pages fail with deterministic unsupported-page mapping.
