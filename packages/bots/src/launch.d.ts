/**
 * Ambient declaration for packages/dev/launch.mjs (plain ESM JS, no types).
 * The real shape is mirrored by LaunchStack in ./testing/harness.ts.
 */
declare module '*.mjs' {
  export function launch(agents?: number): Promise<unknown>
}
