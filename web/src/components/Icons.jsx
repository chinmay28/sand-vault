import React from 'react'

/* The rest of the app writes its icons as characters — ⟳, ✕, ▤ — and that
   works while the meaning has a glyph. These four do not: a bar chart, a
   pencil, a heartbeat and a way out are either missing from the mono faces or
   arrive as full-colour emoji, which sits badly beside 11px monospace. So they
   are drawn instead, on a 16-unit grid, stroked in `currentColor` so each one
   takes the colour of the button it sits in — including the red one.
   `flexShrink: 0` keeps them from being squeezed when a button is narrow. */
function Icon({ children, size = 13 }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      style={{ flexShrink: 0 }}
    >{children}</svg>
  )
}

/* Three bars, tallest in the middle — what the stats sheet opens onto. */
export const StatsIcon = (props) => (
  <Icon {...props}><path d="M3.5 12.5v-3M8 12.5v-9M12.5 12.5v-5" /></Icon>
)

/* A pencil at the usual 45°, its tip in the lower left. */
export const EditIcon = (props) => (
  <Icon {...props}><path d="M10.5 2.8l2.7 2.7M11 2.3l2.7 2.7-7.4 7.4-3.4.7.7-3.4z" /></Icon>
)

/* A trace with one beat in it: the account answered. */
export const TestIcon = (props) => (
  <Icon {...props}><path d="M2 8.5h2.8l1.7-4 2.6 7 1.6-3h3.3" /></Icon>
)

/* An arrow leaving an open-sided box — the way out, not a deletion. */
export const DisconnectIcon = (props) => (
  <Icon {...props}><path d="M6.5 3H3.5v10h3M9.5 5.2L12.4 8l-2.9 2.8M12.2 8H6.2" /></Icon>
)
