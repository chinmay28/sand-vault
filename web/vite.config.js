import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { appVersion } from './build-version.js'

const here = path.dirname(fileURLToPath(import.meta.url))

/* The wordmark's script face.
 *
 * SAND fetches nothing from anywhere: opening your vault makes zero
 * third-party requests, and a logo is not the thing to break that for. So the
 * face is not linked — it is read off disk at build time and embedded in the
 * page itself, which means it cannot become a request to somebody's CDN no
 * matter how the app is deployed.
 *
 * It is also optional. Nefelibata is a licensed font and this repository does
 * not carry one, so when the file is absent the build says so and the wordmark
 * falls back to the platform's own handwriting face (see FONT.script). Nothing
 * breaks; the second half of the name is simply written in a different hand.
 *
 * Drop a licensed copy at web/fonts/nefelibata-script.woff2 — see the README
 * beside it, including how to subset it down to the five letters of "Vault".
 */
const WORDMARK_FAMILY = 'Nefelibata Script'
const WORDMARK_FILES = [
  ['nefelibata-script.woff2', 'woff2', 'font/woff2'],
  ['nefelibata-script.woff', 'woff', 'font/woff'],
]

function wordmarkFont() {
  let announced = false

  return {
    name: 'sand-wordmark-font',
    transformIndexHtml() {
      for (const [name, format, mime] of WORDMARK_FILES) {
        const file = path.resolve(here, 'fonts', name)
        if (!fs.existsSync(file)) continue

        const data = fs.readFileSync(file)
        if (!announced) {
          announced = true
          console.log(`wordmark: embedding ${name} (${Math.round(data.length / 1024)} KB)`)
        }
        return [{
          tag: 'style',
          injectTo: 'head',
          children:
            `@font-face{font-family:'${WORDMARK_FAMILY}';` +
            `src:url(data:${mime};base64,${data.toString('base64')}) format('${format}');` +
            // The wordmark is two words in two hands and one of them is always
            // ready — swap draws it in the fallback hand first rather than
            // holding the whole lockup back for a font that is already local.
            'font-weight:400;font-style:normal;font-display:swap;}',
        }]
      }

      if (!announced) {
        announced = true
        console.log('wordmark: no font at web/fonts — falling back to the system script face')
      }
      return []
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
