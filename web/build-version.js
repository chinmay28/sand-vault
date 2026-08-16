import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

/**
 * The app version, for inlining into the bundle at build time.
 *
 * Delegates to scripts/version.mjs at the repo root — the single place
 * YEAR.MONTH (Go source constants) and PATCH (the git commit count) are
 * assembled, so the web client and the Go binary always report the same
 * number.
 *
 * Shelling out rather than importing keeps the resolution identical to the
 * Makefile's, and it runs once, when Vite loads its config.
 */
export function appVersion() {
  const script = fileURLToPath(new URL('../scripts/version.mjs', import.meta.url));
  return execFileSync(process.execPath, [script], { encoding: 'utf8' }).trim();
}
