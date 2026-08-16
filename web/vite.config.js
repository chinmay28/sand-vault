import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { appVersion } from './build-version.js'

const here = path.dirname(fileURLToPath(import.meta.url))

/* The wordmark's script faces.
 *
 * SAND fetches nothing from anywhere: opening your vault makes zero
 * third-party requests, and a logo is not the thing to break that for. So a
 * face is never linked — it is read off disk at build time and embedded in the
 * page itself, which means it cannot become a request to somebody's CDN no
 * matter how the app is deployed, and there is no second round trip before the
 * mark is drawn.
 *
 * Two faces can be declared, and both are optional:
 *
 *   wordmark-script.woff2   the one in the repository — five glyphs of an
 *                           open-licensed script, under two kilobytes
 *   nefelibata-script.woff2 a licensed face, which the repository cannot carry
 *                           and .gitignore keeps out
 *
 * Which one is actually worn is decided by the order of FONT.script in
 * theme.js, not here. This only declares what the page has to offer, and says
 * on the console what it found; with neither present the wordmark falls back to
 * the platform's own handwriting face and nothing breaks. See the README beside
 * the fonts.
 */
const WORDMARK_FACES = [
  ['Nefelibata Script', 'nefelibata-script'],
  ['SAND Wordmark Script', 'wordmark-script'],
]
const FORMATS = [
  ['woff2', 'font/woff2'],
  ['woff', 'font/woff'],
]

function wordmarkFont() {
  let announced = false

  return {
    name: 'sand-wordmark-font',
    transformIndexHtml() {
      const rules = []
      const found = []

      for (const [family, stem] of WORDMARK_FACES) {
        for (const [format, mime] of FORMATS) {
          const file = path.resolve(here, 'fonts', `${stem}.${format}`)
          if (!fs.existsSync(file)) continue

          const data = fs.readFileSync(file)
          found.push(`${stem}.${format} (${(data.length / 1024).toFixed(1)} KB)`)
          rules.push(
            `@font-face{font-family:'${family}';` +
            `src:url(data:${mime};base64,${data.toString('base64')}) format('${format}');` +
            // The wordmark is two words in two hands and one of them is always
            // ready — swap draws it in the fallback hand first rather than
            // holding the whole lockup back for a face that is already here.
            'font-weight:400;font-style:normal;font-display:swap;}',
          )
          break
        }
      }

      if (!announced) {
        announced = true
        console.log(found.length
          ? `wordmark: embedding ${found.join(', ')}`
          : 'wordmark: no font at web/fonts — falling back to the system script face')
      }

      if (!rules.length) return []
      return [{ tag: 'style', injectTo: 'head', children: rules.join('') }]
    },
  }
}

export default defineConfig({
  // Stamp the version into the bundle — the browser has no git to ask.
  define: { __APP_VERSION__: JSON.stringify(appVersion()) },
  plugins: [react(), wordmarkFont()],
  build: {
    outDir: '../internal/server/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': 'http://localhost:8123',
    },
  },
})
