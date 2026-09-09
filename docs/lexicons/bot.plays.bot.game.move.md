
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.game.move

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.game.move",
  "defs": {
    "main": {
      "type": "record",
      "key": "tid",
      "record": {
        "type": "object",
        "required": [
          "game",
          "ply",
          "payload",
          "receivedAt",
          "moveToken"
        ],
        "properties": {
          "game": {
            "type": "ref",
            "ref": "com.atproto.repo.strongRef"
          },
          "ply": {
            "type": "integer",
            "minimum": 1
          },
          "payload": {
            "type": "union",
            "refs": [
              "bot.plays.bot.chess.move",
              "bot.plays.bot.checkers.move"
            ],
            "closed": false
          },
          "receivedAt": {
            "type": "string",
            "format": "datetime",
            "description": "AppView receipt time, copied from acceptance response"
          },
          "clockRemainingMs": {
            "type": "integer"
          },
          "moveToken": {
            "type": "string",
            "description": "Compact JWS signed by the service DID over {game, ply, payload, receivedAt, player}"
          },
          "position": {
            "type": "union",
            "refs": [
              "bot.plays.bot.chess.position",
              "bot.plays.bot.checkers.position"
            ],
            "closed": false,
            "description": "Optional snapshot after this move"
          }
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->