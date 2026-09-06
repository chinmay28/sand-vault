import React, { useEffect, useState } from 'react'
import { COLORS } from '../theme'
import { api } from '../api'

/* The picture in front of a file's name. It is a stored thumbnail — a small
   JPEG the vault keeps a folder at a time — so drawing one costs nothing like
   rebuilding the file it came from.

   `size` is the edge in pixels: 52 on a phone, where the row is a stack and
   the tile is the left column of it, and 26 on a desktop, where it stands in
   for the emoji inside the Name column without changing the row's height. In
   the grid it is `fill` instead, and the picture is the tile.

   It falls back to that same emoji, and does so on any failure — a file
   uploaded before thumbnails existed, an account that has gone quiet, a pack
   that could not be read. The list has always been readable without pictures.

   Its own module rather than a corner of FileEntry because more than the
   browser's rows draw it: the picker a machine transfer chooses files in is
   one, and that picker is imported *by* FileEntry. */
export function Thumb({ id, icon, size, expected, fill }) {
  const [failed, setFailed] = useState(false)

  // A new file in the same row position must not inherit the old one's state.
  useEffect(() => { setFailed(false) }, [id])

  if (!expected || failed) {
    return (
      <span style={{ flexShrink: 0, fontSize: fill ? '34px' : size >= 40 ? '26px' : '15px' }}>{icon}</span>
    )
  }

  return (
    <img
      src={api.thumbURL(id)}
      alt=""
      width={fill ? undefined : size}
      height={fill ? undefined : size}
      loading="lazy"
      decoding="async"
      onError={() => setFailed(true)}
      style={fill ? {
        width: '100%', height: '100%', objectFit: 'cover', display: 'block',
      } : {
        width: `${size}px`,
        height: `${size}px`,
        flexShrink: 0,
        objectFit: 'cover',
        borderRadius: size >= 40 ? '6px' : '4px',
        background: COLORS.surfaceRaised,
        border: `1px solid ${COLORS.border}`,
      }}
    />
  )
}
