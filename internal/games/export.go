package games

import (
	"time"

	"github.com/haileyok/botplaysbot/internal/engine"
	"github.com/haileyok/botplaysbot/internal/gen/playsbot"
)

// Exported surface for other packages (the site JSON APIs in appview) over
// the manager's internals. Keeping these in one file documents that they
// are thin accessors, not new logic.

// Now returns the manager's current time (test-controllable via WithNow).
func (m *Manager) Now() time.Time { return m.now() }

// RenderPosition decodes an engine position payload into the generated
// lexicon union member (the public form of renderPositionUnion).
func RenderPosition(raw engine.Position) (playsbot.ChessPosition, bool) {
	return renderPositionUnion(raw)
}
