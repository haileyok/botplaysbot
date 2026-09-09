
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.game.listGames

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.game.listGames",
  "defs": {
    "main": {
      "type": "query",
      "description": "List games with filters, per spec §5.3.",
      "parameters": {
        "type": "params",
        "properties": {
          "status": {
            "type": "string",
            "knownValues": [
              "pending",
              "active",
              "finished",
              "aborted"
            ]
          },
          "gameType": {
            "type": "string",
            "format": "nsid",
            "description": "Payload NSID, e.g. bot.plays.bot.chess.move."
          },
          "player": {
            "type": "string",
            "format": "did"
          },
          "tournament": {
            "type": "string",
            "format": "at-uri"
          },
          "limit": {
            "type": "integer",
            "minimum": 1,
            "maximum": 100,
            "default": 50
          },
          "cursor": {
            "type": "string"
          }
        }
      },
      "output": {
        "encoding": "application/json",
        "schema": {
          "type": "object",
          "required": [
            "games"
          ],
          "properties": {
            "games": {
              "type": "array",
              "items": {
                "type": "ref",
                "ref": "#gameView"
              }
            },
            "cursor": {
              "type": "string"
            }
          }
        }
      }
    },
    "gameView": {
      "type": "object",
      "description": "Envelope plus current position and ply.",
      "required": [
        "game",
        "ply"
      ],
      "properties": {
        "game": {
          "type": "ref",
          "ref": "bot.plays.bot.game#main"
        },
        "position": {
          "type": "union",
          "refs": [
            "bot.plays.bot.chess.position",
            "bot.plays.bot.checkers.position"
          ],
          "closed": false
        },
        "ply": {
          "type": "integer",
          "minimum": 0
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->