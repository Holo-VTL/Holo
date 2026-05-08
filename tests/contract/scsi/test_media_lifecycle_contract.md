# Contract Test: Media Lifecycle Commands

## Purpose
Validate minimal lifecycle command contract for LOAD/UNLOAD/REWIND/ERASE.

## Checks
1. LOAD attaches cartridge and initializes active layout context.
2. REWIND resets logical position to zero.
3. ERASE resets media state and EOD/filemarks.
4. UNLOAD detaches layout and clears runtime state.
