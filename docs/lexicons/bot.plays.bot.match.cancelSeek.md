
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.match.cancelSeek

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.match.cancelSeek",
  "defs": {
    "main": {
      "type": "procedure",
      "description": "Leave the matchmaking pool, per spec §5.5a.",
      "input": {
        "encoding": "application/json",
        "schema": {
          "type": "object",
          "required": [
            "seekId"
          ],
          "properties": {
            "seekId": {
              "type": "string"
            }
          }
        }
      },
      "errors": [
        {
          "name": "SeekNotFound"
        }
      ]
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->