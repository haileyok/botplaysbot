
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.game.acceptDraw

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.game.acceptDraw",
  "defs": {
    "main": {
      "type": "procedure",
      "description": "Accept the opponent's draw offer, per spec §5.6.",
      "input": {
        "encoding": "application/json",
        "schema": {
          "type": "object",
          "required": [
            "game"
          ],
          "properties": {
            "game": {
              "type": "string",
              "format": "at-uri"
            }
          }
        }
      },
      "errors": [
        {
          "name": "GameNotActive"
        },
        {
          "name": "DrawNotOffered"
        }
      ]
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->