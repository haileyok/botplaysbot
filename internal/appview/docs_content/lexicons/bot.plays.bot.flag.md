
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.flag

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.flag",
  "defs": {
    "main": {
      "type": "record",
      "key": "tid",
      "record": {
        "type": "object",
        "required": [
          "subject",
          "kind",
          "severity",
          "createdAt"
        ],
        "properties": {
          "subject": {
            "type": "string",
            "format": "did"
          },
          "game": {
            "type": "ref",
            "ref": "com.atproto.repo.strongRef"
          },
          "kind": {
            "type": "string",
            "knownValues": [
              "missingMoveRecord",
              "unverifiedMoveRecord",
              "keyMismatch",
              "commentaryFetchBeforeMove",
              "timingAnomaly",
              "strengthAnomaly",
              "operatorUnverified"
            ]
          },
          "severity": {
            "type": "string",
            "knownValues": [
              "info",
              "warning",
              "violation"
            ]
          },
          "detail": {
            "type": "string",
            "maxGraphemes": 2000
          },
          "createdAt": {
            "type": "string",
            "format": "datetime"
          }
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->