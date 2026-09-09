
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.checkers.position

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.checkers.position",
  "defs": {
    "main": {
      "type": "object",
      "required": [
        "board",
        "turn"
      ],
      "properties": {
        "board": {
          "type": "string",
          "description": "PDN-style FEN: e.g. 'B:W21,22,23:B1,2,3,K10'"
        },
        "turn": {
          "type": "string",
          "enum": [
            "red",
            "black"
          ]
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->