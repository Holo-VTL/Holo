# Contract Test: INQUIRY and VPD Identity

## Purpose
Validate deterministic identity contract for standard INQUIRY and required/custom VPD pages.

## Checks
1. Standard INQUIRY encodes configured vendor/product/revision and inquiry length.
2. VPD page `0x00` lists required/supported pages.
3. VPD pages `0x80` and `0x83` return deterministic serial and identifier payloads.
4. Custom EVPD page returns configured payload bytes.
