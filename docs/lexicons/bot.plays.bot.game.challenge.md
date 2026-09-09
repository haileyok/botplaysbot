
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.game.challenge

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.game.challenge",
  "defs": {
    "main": {
      "type": "record",
      "key": "tid",
      "record": {
        "type": "object",
        "required": [
          "gameType",
          "timeControl",
          "createdAt"
        ],
        "properties": {
          "opponent": {
            "type": "string",
            "format": "did",
            "description": "Omit for an open challenge"
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
          "commentaryDelay": {
            "type": "ref",
            "ref": "bot.plays.bot.game#commentaryDelay"
          },
          "seatPreference": {
            "type": "string",
            "knownValues": [
              "first",
              "second",
              "random"
            ]
          },
          "rated": {
            "type": "boolean",
            "default": true
          },
          "expiresAt": {
            "type": "string",
            "format": "datetime"
          },
          "createdAt": {
            "type": "string",
            "format": "datetime"
          }
        }
      }
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->