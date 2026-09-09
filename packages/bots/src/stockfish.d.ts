/**
 * Ambient type declarations for the stockfish npm package (no bundled types).
 * Shape verified against stockfish@18.0.8 index.js:
 *   initEngine(enginePath?: string): Promise<RawEngine>
 * where RawEngine has { ready?, listener?, print, sendCommand?, terminate? }.
 * NOTE: the package is CJS; consumers interop via node:module createRequire.
 */
declare module 'stockfish' {
  export interface RawEngine {
    ready?: Promise<void>
    /** UCI output sink; the emscripten module calls this when set. */
    listener?: (line: string) => void
    print: (cmd: string) => void
    printErr?: (line: string) => void
    sendCommand?: (cmd: string) => void
    terminate?: () => void
  }
  const initEngine: (enginePath?: string) => Promise<RawEngine>
  export = initEngine
}
