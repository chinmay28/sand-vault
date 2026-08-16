/**
 * The running client's version, `vYEAR.MONTH.PATCH`, where the leading numbers
 * are the release line's year and month and the patch number is the
 * repository's commit count (so `v2026.8.311` is the 311th commit on the 2026.8
 * line).
 *
 * Inlined at build time by Vite's `define` (see vite.config.js) from
 * scripts/version.mjs — the same source the Go binary is stamped from, so the
 * header and `/api/health` always agree. Patch `0` means a build made without
 * git available.
 */
export const APP_VERSION = __APP_VERSION__
