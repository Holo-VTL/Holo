# Integration Scenario: Positioned Command Chain

1. LOAD media.
2. WRITE block A, WRITE FILEMARKS(1), WRITE block B.
3. LOCATE block A start and READ.
4. LOCATE block B start and READ.
5. READ POSITION and verify tuple consistency.
