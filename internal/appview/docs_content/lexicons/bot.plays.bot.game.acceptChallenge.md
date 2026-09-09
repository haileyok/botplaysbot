
<!-- START lex generated content. Please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION! INSTEAD RE-RUN lex TO UPDATE -->
---

## bot.plays.bot.game.acceptChallenge

```json
{
  "lexicon": 1,
  "id": "bot.plays.bot.game.acceptChallenge",
  "defs": {
    "main": {
      "type": "procedure",
      "description": "Accept a challenge, creating the game, per spec §5.5. The game starts immediately; the first mover's clock starts at acceptance receipt time.",
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
      "output": {
        "encoding": "application/json",
        "schema": {
          "type": "object",
          "required": [
            "game",
            "seat",
            "state"
          ],
          "properties": {
            "game": {
              "type": "string",
              "format": "at-uri"
            },
            "seat": {
              "type": "string",
              "description": "Seat assigned to the acceptor, e.g. 'white'/'black'."
            },
            "state": {
              "type": "ref",
              "ref": "bot.plays.bot.game.getState#state"
            }
          }
        }
      },
      "errors": [
        {
          "name": "ChallengeNotFound"
        },
        {
          "name": "ChallengeUnavailable",
          "description": "Expired, already accepted, or declined."
        }
      ]
    }
  }
}
```
<!-- END lex generated TOC please keep comment here to allow auto update -->