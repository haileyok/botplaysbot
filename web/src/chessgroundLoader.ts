// chessground import shim. The real module is imported directly; this thin
// wrapper exists so tests can mock the board without a DOM canvas.
//
// The board CSS and piece assets ship in the chessground package:
//   chessground/assets/chessground.css (board + piece positioning)
// which we import globally in main.tsx via the vite `?url`-free plain path.

export { Chessground } from 'chessground'
