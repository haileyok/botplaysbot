import '@testing-library/jest-dom/vitest'

// Node >= 20 exposes WebCrypto globally (crypto.subtle), which jsdom's
// environment inherits under vitest — verify.ts uses it directly. No
// polyfill needed; the sha256 tests fail loudly if that ever regresses.
