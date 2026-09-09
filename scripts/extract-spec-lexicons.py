#!/usr/bin/env python3
"""Extract the 12 verbatim record/object lexicon JSONs from docs/spec-v0.1.md (§4.3–4.11).

The JSON blocks in the spec are the source of truth and must be copied
character-for-character. This script slices them out of the markdown fences
and writes them to lexicons/bot/plays/bot/... mirroring their NSID paths.
"""

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SPEC = ROOT / "docs" / "spec-v0.1.md"
LEXDIR = ROOT / "lexicons"

# section heading -> output path (relative to LEXDIR)
TARGETS = {
    "bot.plays.bot.actor.profile": "bot/plays/bot/actor/profile.json",
    "bot.plays.bot.game": "bot/plays/bot/game.json",
    "bot.plays.bot.game.move": "bot/plays/bot/game/move.json",
    "bot.plays.bot.game.commentary": "bot/plays/bot/game/commentary.json",
    "bot.plays.bot.game.reveal": "bot/plays/bot/game/reveal.json",
    "bot.plays.bot.game.challenge": "bot/plays/bot/game/challenge.json",
    "bot.plays.bot.rating": "bot/plays/bot/rating.json",
    "bot.plays.bot.flag": "bot/plays/bot/flag.json",
    "bot.plays.bot.chess.move": "bot/plays/bot/chess/move.json",
    "bot.plays.bot.chess.position": "bot/plays/bot/chess/position.json",
    "bot.plays.bot.checkers.move": "bot/plays/bot/checkers/move.json",
    "bot.plays.bot.checkers.position": "bot/plays/bot/checkers/position.json",
}


def main() -> int:
    text = SPEC.read_text(encoding="utf-8")
    fences = re.findall(r"```json\n(.*?)\n```", text, flags=re.DOTALL)
    by_id = {}
    for block in fences:
        m = re.search(r'"id"\s*:\s*"([^"]+)"', block)
        if not m:
            continue
        nsid = m.group(1)
        if nsid in by_id:
            sys.exit(f"duplicate fenced lexicon id {nsid!r} in spec")
        by_id[nsid] = block

    missing = [nsid for nsid in TARGETS if nsid not in by_id]
    if missing:
        sys.exit(f"spec is missing fenced lexicons for: {missing}")

    for nsid, relpath in TARGETS.items():
        out = LEXDIR / relpath
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(by_id[nsid] + "\n", encoding="utf-8")
        print(f"wrote {out.relative_to(ROOT)} ({nsid})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
