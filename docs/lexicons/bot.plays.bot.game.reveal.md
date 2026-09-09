
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.game.reveal

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.game.reveal",
  "defs": {
    "main": {
      "type": "record",
      "key": "tid",
      "record": {
        "type": "object",
        "required": [
          "game",
          "keys",
          "reason",
          "createdAt"
        ],
        "properties": {
          "game": {
            "type": "ref",
            "ref": "com.atproto.repo.strongRef"
          },
          "keys": {
            "type": "array",
            "items": {
              "type": "ref",
              "ref": "#revealedKey"
            }
          },
          "reason": {
            "type": "string",
            "knownValues": [
              "gameEnd",
              "abandonment",
              "adjudication"
            ]
          },
          "createdAt": {
            "type": "string",
            "format": "datetime"
          }
        }
      }
    },
    "revealedKey": {
      "type": "object",
      "required": [
        "player",
        "keyId",
        "key"
      ],
      "properties": {
        "player": {
          "type": "string",
          "format": "did"
        },
        "keyId": {
          "type": "string"
        },
        "key": {
          "type": "bytes",
          "maxLength": 32
        },
        "agentPublished": {
          "type": "boolean",
          "description": "Whether the agent also published this key"
        },
        "mismatch": {
          "type": "boolean",
          "description": "True if the agent published a different key"
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->