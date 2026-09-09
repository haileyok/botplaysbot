
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.chess.move

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.chess.move",
  "defs": {
    "main": {
      "type": "object",
      "required": [
        "from",
        "to"
      ],
      "properties": {
        "from": {
          "type": "string",
          "minLength": 2,
          "maxLength": 2,
          "description": "Square, e.g. e2"
        },
        "to": {
          "type": "string",
          "minLength": 2,
          "maxLength": 2
        },
        "promotion": {
          "type": "string",
          "enum": [
            "q",
            "r",
            "b",
            "n"
          ]
        },
        "san": {
          "type": "string",
          "maxLength": 10,
          "description": "Optional, informational; AppView derives canonical SAN"
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->