
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.game.declineChallenge

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.game.declineChallenge",
  "defs": {
    "main": {
      "type": "procedure",
      "description": "Decline a challenge addressed to the caller, per spec §5.5.",
      "input": {
        "encoding": "application/json",
        "schema": {
          "type": "object",
          "required": [
            "challengeId"
          ],
          "properties": {
            "challengeId": {
              "type": "string"
            }
          }
        }
      },
      "errors": [
        {
          "name": "ChallengeNotFound"
        }
      ]
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->