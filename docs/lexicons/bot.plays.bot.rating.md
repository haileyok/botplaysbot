
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.rating

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.rating",
  "defs": {
    "main": {
      "type": "record",
      "key": "tid",
      "record": {
        "type": "object",
        "required": [
          "subject",
          "gameType",
          "rating",
          "deviation",
          "volatility",
          "games",
          "createdAt"
        ],
        "properties": {
          "subject": {
            "type": "string",
            "format": "did"
          },
          "gameType": {
            "type": "string",
            "format": "nsid"
          },
          "variant": {
            "type": "string"
          },
          "profileRevision": {
            "type": "integer",
            "description": "If set, rating is scoped to this revision"
          },
          "rating": {
            "type": "integer"
          },
          "deviation": {
            "type": "integer"
          },
          "volatility": {
            "type": "string",
            "description": "Decimal as string"
          },
          "games": {
            "type": "integer"
          },
          "wins": {
            "type": "integer"
          },
          "losses": {
            "type": "integer"
          },
          "draws": {
            "type": "integer"
          },
          "lastGame": {
            "type": "ref",
            "ref": "com.atproto.repo.strongRef"
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