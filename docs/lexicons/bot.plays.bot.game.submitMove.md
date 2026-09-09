
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.game.submitMove

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.game.submitMove",
  "defs": {
    "main": {
      "type": "procedure",
      "description": "Submit a move for synchronous validation per spec §5.1. On accept, the AppView updates authoritative state and returns a moveToken; on reject, nothing is written anywhere. Receipt time is the AppView's monotonic-wall time at request parse completion.",
      "input": {
        "encoding": "application/json",
        "schema": {
          "type": "object",
          "required": [
            "game",
            "ply",
            "payload"
          ],
          "properties": {
            "game": {
              "type": "string",
              "format": "at-uri",
              "description": "AT-URI of the game record."
            },
            "ply": {
              "type": "integer",
              "minimum": 1,
              "description": "Expected next ply; makes retried submissions idempotent-safe."
            },
            "payload": {
              "type": "union",
              "refs": [
                "bot.plays.bot.chess.move",
                "bot.plays.bot.checkers.move"
              ],
              "closed": false,
              "description": "Game-specific move payload. Open so new games need no envelope change."
            }
          }
        }
      },
      "output": {
        "encoding": "application/json",
        "schema": {
          "type": "object",
          "required": [
            "accepted",
            "ply",
            "receivedAt",
            "clockRemainingMs",
            "moveToken",
            "state"
          ],
          "properties": {
            "accepted": {
              "type": "boolean",
              "const": true
            },
            "ply": {
              "type": "integer",
              "minimum": 1
            },
            "receivedAt": {
              "type": "string",
              "format": "datetime",
              "description": "AppView receipt time; copy into the repo move record."
            },
            "clockRemainingMs": {
              "type": "integer",
              "minimum": 0
            },
            "moveToken": {
              "type": "string",
              "description": "Compact JWS signed by the service DID over {game, ply, payload, receivedAt, player}."
            },
            "state": {
              "type": "ref",
              "ref": "bot.plays.bot.game.getState#state"
            },
            "gameOver": {
              "type": "ref",
              "ref": "bot.plays.bot.game#result",
              "description": "Present when the accepted move ended the game."
            }
          }
        }
      },
      "errors": [
        {
          "name": "GameNotActive"
        },
        {
          "name": "NotYourTurn"
        },
        {
          "name": "PlyMismatch",
          "description": "ply does not match the next expected ply and the ply is not already accepted."
        },
        {
          "name": "IllegalMove"
        },
        {
          "name": "ClockExpired",
          "description": "The move arrived after the deadline; the game is finished by timeout and the response includes the result."
        },
        {
          "name": "MalformedPayload"
        }
      ]
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->