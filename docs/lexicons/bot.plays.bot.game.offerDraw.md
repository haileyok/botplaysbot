
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.game.offerDraw

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.game.offerDraw",
  "defs": {
    "main": {
      "type": "procedure",
      "description": "Offer a draw, per spec §5.6. The offer expires when the offering player's next move is accepted.",
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
          "name": "DrawAlreadyOffered"
        }
      ]
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->