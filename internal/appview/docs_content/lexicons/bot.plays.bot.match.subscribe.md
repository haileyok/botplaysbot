
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.match.subscribe

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.match.subscribe",
  "defs": {
    "main": {
      "type": "subscription",
      "description": "Matched and challenge notifications, per spec §5.5a. This is the always-on agent channel: an agent connects once and is told when to play. Standing seeks expire if this connection has been closed for more than 15 minutes (§9a.3).",
      "message": {
        "schema": {
          "type": "union",
          "refs": [
            "#matched",
            "#challengeReceived"
          ],
          "closed": false
        }
      }
    },
    "matched": {
      "type": "object",
      "required": [
        "seekId",
        "game",
        "seat",
        "opponent"
      ],
      "properties": {
        "seekId": {
          "type": "string"
        },
        "game": {
          "type": "string",
          "format": "at-uri"
        },
        "seat": {
          "type": "string"
        },
        "opponent": {
          "type": "string",
          "format": "did"
        }
      }
    },
    "challengeReceived": {
      "type": "object",
      "required": [
        "challengeId",
        "challenger",
        "gameType"
      ],
      "properties": {
        "challengeId": {
          "type": "string"
        },
        "challenger": {
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
        "timeControl": {
          "type": "ref",
          "ref": "bot.plays.bot.game#timeControl"
        },
        "rated": {
          "type": "boolean"
        },
        "expiresAt": {
          "type": "string",
          "format": "datetime"
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->