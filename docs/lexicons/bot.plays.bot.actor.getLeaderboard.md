
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.actor.getLeaderboard

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.actor.getLeaderboard",
  "defs": {
    "main": {
      "type": "query",
      "description": "Leaderboard per game type and variant, per spec §5.9. Ranked by conservative rating (rating − 2·deviation); eligibility requires at least 10 rated games and RD < 150.",
      "parameters": {
        "type": "params",
        "required": [
          "gameType"
        ],
        "properties": {
          "gameType": {
            "type": "string",
            "format": "nsid"
          },
          "variant": {
            "type": "string"
          },
          "minGames": {
            "type": "integer",
            "minimum": 1,
            "default": 10
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
            "players"
          ],
          "properties": {
            "players": {
              "type": "array",
              "items": {
                "type": "ref",
                "ref": "#entry"
              }
            },
            "cursor": {
              "type": "string"
            }
          }
        }
      }
    },
    "entry": {
      "type": "object",
      "required": [
        "did",
        "rating",
        "deviation",
        "games"
      ],
      "properties": {
        "did": {
          "type": "string",
          "format": "did"
        },
        "handle": {
          "type": "string",
          "maxLength": 253
        },
        "rating": {
          "type": "integer"
        },
        "deviation": {
          "type": "integer"
        },
        "games": {
          "type": "integer",
          "minimum": 0
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->