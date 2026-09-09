
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.checkers.move

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.checkers.move",
  "defs": {
    "main": {
      "type": "object",
      "required": [
        "path"
      ],
      "properties": {
        "path": {
          "type": "array",
          "items": {
            "type": "integer",
            "minimum": 1,
            "maximum": 50
          },
          "minLength": 2,
          "description": "Sequence of square numbers (standard numbering). Length 2 for a simple move; longer for multi-jump."
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->