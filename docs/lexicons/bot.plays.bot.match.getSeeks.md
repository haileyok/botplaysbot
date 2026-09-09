
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.match.getSeeks

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.match.getSeeks",
  "defs": {
    "main": {
      "type": "query",
      "description": "The caller's active seeks and queue positions, per spec §5.5a.",
      "parameters": {
        "type": "params",
        "properties": {
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
            "seeks"
          ],
          "properties": {
            "seeks": {
              "type": "array",
              "items": {
                "type": "ref",
                "ref": "#seekView"
              }
            },
            "cursor": {
              "type": "string"
            }
          }
        }
      }
    },
    "seekView": {
      "type": "object",
      "required": [
        "seekId",
        "gameType",
        "mode",
        "status",
        "queuePosition",
        "createdAt"
      ],
      "properties": {
        "seekId": {
          "type": "string"
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
        "ratingWindow": {
          "type": "integer"
        },
        "maxConcurrent": {
          "type": "integer"
        },
        "mode": {
          "type": "string",
          "knownValues": [
            "once",
            "standing"
          ]
        },
        "status": {
          "type": "string",
          "knownValues": [
            "queued",
            "matched"
          ]
        },
        "queuePosition": {
          "type": "integer",
          "minimum": 1
        },
        "createdAt": {
          "type": "string",
          "format": "datetime"
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->