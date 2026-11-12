/* SAND TV — one folder of films, driven with a remote.

   The page talks to /api/tv/* with the device token it got by pairing, and to
   nothing else. It is plain ES2017 with no build step, because it is embedded
   in the server as it stands and runs on whatever browser engine a TV has.

   The Fire TV app is a WebView around this page. It adds two things a page
   cannot do on its own, through window.SandTVNative:

     play(url, title, startMs, itemId)   hand a film to the native player,
                                         which plays far more formats than
                                         <video> does (MKV, HEVC, AC-3…)
     changeServer()                      go back to the app's server list

   and calls back into window.sandTV:

     back()                              → true if the page handled Back
     onPlaybackStopped(itemId, posMs, durMs)

   Without the app, films play in the browser's own <video>. */

(function () {
  'use strict'

  var native = window.SandTVNative || null
  var KEY_TOKEN = 'sandtv.token'
  var CODE_ALPHABET = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  var CODE_LENGTH = 4

  function $(id) { return document.getElementById(id) }

  /* --- Storage: tolerant of a browser that refuses it ------------------- */

  function load(key) {
    try { return window.localStorage.getItem(key) } catch (e) { return null }
  }
  function save(key, value) {
    try {
      if (value === null || value === undefined) window.localStorage.removeItem(key)
      else window.localStorage.setItem(key, value)
    } catch (e) { /* a TV without storage pairs every time; nothing worse */ }
  }

  /* Where each film was stopped, so Play offers to resume. Kept on the TV, not
     the server: it is this screen's memory of what was watched on it. */
  function progressOf(id) {
    try { return JSON.parse(load('sandtv.pos.' + id)) || null } catch (e) { return null }
  }
  function setProgress(id, posMs, durMs) {
    if (!id) return
    if (!durMs || posMs < 60000) {
      // Barely started: not worth offering to resume.
      if (!progressOf(id) || !progressOf(id).watched) save('sandtv.pos.' + id, null)
      return
    }
    var watched = posMs >= durMs * 0.92
    save('sandtv.pos.' + id, JSON.stringify({ pos: watched ? 0 : posMs, dur: durMs, watched: watched }))
  }

  /* --- API ---------------------------------------------------------------- */

  var token = load(KEY_TOKEN)

  function ApiError(status, body) {
    this.status = status
    this.code = (body && body.code) || ''
    this.message = (body && body.error) || ('Request failed (' + status + ')')
  }

  function api(method, path, body) {
    var headers = { 'Accept': 'application/json' }
    if (token) headers['Authorization'] = 'Bearer ' + token
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    return fetch(path, {
      method: method,
      headers: headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
    }).then(function (resp) {
      return resp.text().then(function (text) {
        var data = null
        try { data = text ? JSON.parse(text) : null } catch (e) { data = null }
        if (!resp.ok) throw new ApiError(resp.status, data)
        return data
      })
    }, function () {
      throw new ApiError(0, { code: 'OFFLINE', error: 'Cannot reach SAND — is the server still running?' })
    })
  }

  /* Every failure lands here, so the two that mean "change screen" are
     handled the same way wherever they come from. */
  function handleError(err) {
    if (err && err.code === 'TV_UNPAIRED') {
      unpairLocally()
      showPair(err.message)
      return
    }
    if (err && err.code === 'LOCKED') {
      showLocked()
      return
    }
    toast((err && err.message) || 'Something went wrong')
  }

  /* --- Pictures: fetched with the token, kept as blobs -------------------
     An <img> cannot send the Authorization header, so posters and folder
     pictures are fetched and handed to it as blob: URLs, cached by key for as
     long as the page is open. */

  var imageURLs = {}
  var imageQueue = []
  var imagesInFlight = 0

  function posterFor(id) {
    return imageFor('item:' + id, '/api/tv/items/' + encodeURIComponent(id) + '/poster')
  }

  function folderArtFor(path) {
    return imageFor('folder:' + path, '/api/tv/folders/art?path=' + encodeURIComponent(path))
  }

  function imageFor(key, url) {
    if (imageURLs[key]) return Promise.resolve(imageURLs[key])
    return new Promise(function (resolve) {
      imageQueue.push({ key: key, url: url, resolve: resolve })
      pumpImages()
    })
  }

  /* Four at a time: the first picture of a folder gathers its whole pack from
     the clouds, and the rest of the grid should not queue up behind a TV's
     connection limit while it does. */
  function pumpImages() {
    while (imagesInFlight < 4 && imageQueue.length) {
      var job = imageQueue.shift()
      imagesInFlight++
      fetchImage(job.key, job.url).then(job.resolve, function () { job.resolve(null) }).then(function () {
        imagesInFlight--
        pumpImages()
      })
    }
  }

  function fetchImage(key, url) {
    if (imageURLs[key]) return Promise.resolve(imageURLs[key])
    return fetch(url, { headers: { 'Authorization': 'Bearer ' + token } }).then(function (resp) {
      if (!resp.ok) return null
      return resp.blob().then(function (blob) {
        imageURLs[key] = URL.createObjectURL(blob)
        return imageURLs[key]
      })
    })
  }

  /* --- Screens ------------------------------------------------------------ */

  var current = 'loading'
  var lastFocus = {}

  function show(name, focus) {
    if (current && document.activeElement && document.activeElement !== document.body) {
      lastFocus[current] = document.activeElement
    }
    var screens = document.querySelectorAll('.screen')
    for (var i = 0; i < screens.length; i++) {
      screens[i].hidden = screens[i].id !== 'screen-' + name
    }
    current = name
    var target = focus || firstFocusable($('screen-' + name))
    if (target) target.focus()
  }

  function firstFocusable(root) {
    return root.querySelector('[data-focus]:not([hidden]):not([disabled])')
  }

  var toastTimer = null
  function toast(message) {
    var el = $('toast')
    el.textContent = message
    el.hidden = false
    clearTimeout(toastTimer)
    toastTimer = setTimeout(function () { el.hidden = true }, 4500)
  }

  /* --- Spatial navigation -------------------------------------------------
     Arrow keys move focus to the nearest focusable thing in that direction,
     measured between the edges of the two boxes, with a penalty for drifting
     sideways so that "down" in a grid means the card below, not the one
     diagonally nearest. */

  function focusables() {
    var root = $('screen-' + current)
    var all = root.querySelectorAll('[data-focus]')
    var out = []
    for (var i = 0; i < all.length; i++) {
      var el = all[i]
      if (el.disabled || el.hidden || el.offsetParent === null) continue
      out.push(el)
    }
    return out
  }

  function move(dir) {
    var from = document.activeElement
    var candidates = focusables()
    if (!from || candidates.indexOf(from) < 0) {
      if (candidates[0]) candidates[0].focus()
      return
    }
    var a = from.getBoundingClientRect()
    var best = null
    var bestScore = Infinity
    for (var i = 0; i < candidates.length; i++) {
      var el = candidates[i]
      if (el === from) continue
      var b = el.getBoundingClientRect()
      var along, across
      if (dir === 'left') { along = a.left - b.right; across = overlapGap(a.top, a.bottom, b.top, b.bottom) }
      if (dir === 'right') { along = b.left - a.right; across = overlapGap(a.top, a.bottom, b.top, b.bottom) }
      if (dir === 'up') { along = a.top - b.bottom; across = overlapGap(a.left, a.right, b.left, b.right) }
      if (dir === 'down') { along = b.top - a.bottom; across = overlapGap(a.left, a.right, b.left, b.right) }
      if (along < -2) continue
      var score = Math.max(along, 0) + across * 3
      if (score < bestScore) { bestScore = score; best = el }
    }
    if (best) {
      best.focus()
      if (best.scrollIntoView) best.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
    }
  }

  /* How far apart two spans are on the cross axis, zero when they overlap. */
  function overlapGap(a1, a2, b1, b2) {
    if (b2 < a1) return a1 - b2
    if (b1 > a2) return b1 - a2
    return 0
  }

  /* --- Pairing ------------------------------------------------------------ */

  var typed = ''

  function buildKeypad() {
    var pad = $('keypad')
    for (var i = 0; i < CODE_ALPHABET.length; i++) {
      var key = document.createElement('button')
      key.className = 'key'
      key.setAttribute('data-focus', '')
      key.textContent = CODE_ALPHABET[i]
      key.addEventListener('click', typeChar.bind(null, CODE_ALPHABET[i]))
      pad.appendChild(key)
    }
  }

  function typeChar(c) {
    if (typed.length >= CODE_LENGTH) return
    typed += c
    drawCode()
    if (typed.length === CODE_LENGTH) {
      $('pair-connect').focus()
    }
  }

  function deleteChar() {
    typed = typed.slice(0, -1)
    drawCode()
  }

  function drawCode() {
    var boxes = $('code-boxes').children
    for (var i = 0; i < boxes.length; i++) {
      boxes[i].textContent = typed[i] || ''
      boxes[i].className = 'code-box' + (typed[i] ? ' filled' : i === typed.length ? ' next' : '')
    }
  }

  function showPair(message) {
    typed = ''
    drawCode()
    var err = $('pair-error')
    err.textContent = message || ''
    err.hidden = !message
    show('pair', $('keypad').firstChild)
  }

  var pairing = false
  function connect() {
    if (pairing) return
    if (typed.length !== CODE_LENGTH) {
      toast('The code is ' + CODE_LENGTH + ' characters')
      return
    }
    pairing = true
    api('POST', '/api/tv/pair', { code: typed }).then(function (resp) {
      token = resp.token
      save(KEY_TOKEN, token)
      openLibrary('/', false)
    }, function (err) {
      typed = ''
      drawCode()
      var box = $('pair-error')
      box.textContent = err.message
      box.hidden = false
      $('keypad').firstChild.focus()
    }).then(function () { pairing = false })
  }

  function unpairLocally() {
    token = null
    save(KEY_TOKEN, null)
    for (var key in imageURLs) URL.revokeObjectURL(imageURLs[key])
    imageURLs = {}
  }

  /* --- Locked ------------------------------------------------------------- */

  var lockedTimer = null

  function showLocked() {
    show('locked')
    clearInterval(lockedTimer)
    // Asking whether the vault is open again does not count as using it, so
    // a TV left on this screen holds nothing open.
    lockedTimer = setInterval(checkUnlocked, 10000)
  }

  function checkUnlocked() {
    api('GET', '/api/tv/session').then(function (s) {
      if (!s.locked) {
        clearInterval(lockedTimer)
        openLibrary(library.path || '/', library.all)
      }
    }, function (err) {
      if (err.code === 'TV_UNPAIRED') {
        clearInterval(lockedTimer)
        handleError(err)
      }
    })
  }

  /* --- Library ------------------------------------------------------------ */

  var library = { path: '/', all: false, label: '', parent: '' }

  function openLibrary(path, all, focusId) {
    return api('GET', '/api/tv/library?path=' + encodeURIComponent(path) + (all ? '&all=1' : '')).then(function (data) {
      library = { path: data.path, all: all, label: data.label, parent: data.parent || '' }
      drawLibrary(data, focusId)
    }, handleError)
  }

  function drawLibrary(data, focusId) {
    var crumbs = $('crumbs')
    crumbs.textContent = ''
    var strong = document.createElement('strong')
    strong.textContent = data.label
    crumbs.appendChild(strong)
    if (data.path !== '/' && !library.all) crumbs.appendChild(document.createTextNode('  ›  ' + data.path.slice(1).split('/').join('  ›  ')))

    $('tab-folders').classList.toggle('active', !library.all)
    $('tab-all').classList.toggle('active', library.all)

    var grid = $('grid')
    grid.textContent = ''
    var focusTarget = null

    data.folders.forEach(function (f) {
      var card = makeCard('▸', f.name, f.count + (f.count === 1 ? ' video' : ' videos'), 'card-folder')
      if (f.art) attachImage(card.firstChild, folderArtFor(f.path))
      card.addEventListener('click', function () { openLibrary(f.path, false) })
      grid.appendChild(card)
    })

    data.items.forEach(function (item) {
      var title = item.title || stripExtension(item.name)
      var sub = item.year ? String(item.year) : (item.folder !== '/' && library.all ? item.folder.slice(1) : '')
      var card = makeCard('🎬', title, sub, '')
      var art = card.firstChild
      var progress = progressOf(item.id)
      if (progress && progress.watched) {
        var badge = document.createElement('span')
        badge.className = 'card-watched'
        badge.textContent = '✓'
        art.appendChild(badge)
      } else if (progress && progress.dur) {
        var bar = document.createElement('div')
        bar.className = 'card-progress'
        var fill = document.createElement('div')
        fill.style.width = Math.min(100, Math.round(progress.pos / progress.dur * 100)) + '%'
        bar.appendChild(fill)
        art.appendChild(bar)
      }
      if (item.poster) attachImage(art, posterFor(item.id))
      card.addEventListener('click', function () { openDetails(item.id) })
      if (item.id === focusId) focusTarget = card
      grid.appendChild(card)
    })

    $('library-empty').hidden = data.folders.length + data.items.length > 0
    grid.scrollTop = 0
    show('library', focusTarget || firstFocusable(grid) || $('tab-folders'))
    if (focusTarget && focusTarget.scrollIntoView) focusTarget.scrollIntoView({ block: 'center' })
  }

  function makeCard(glyph, name, sub, extra) {
    var card = document.createElement('button')
    card.className = 'card ' + extra
    card.setAttribute('data-focus', '')

    var art = document.createElement('div')
    art.className = 'card-art'
    var g = document.createElement('span')
    g.className = 'card-glyph'
    g.textContent = glyph
    art.appendChild(g)
    card.appendChild(art)

    var n = document.createElement('div')
    n.className = 'card-name'
    n.textContent = name
    card.appendChild(n)

    var s = document.createElement('div')
    s.className = 'card-sub'
    s.textContent = sub || ' '
    card.appendChild(s)
    return card
  }

  function attachImage(art, picture) {
    picture.then(function (url) {
      if (!url) return
      var img = document.createElement('img')
      img.alt = ''
      img.src = url
      art.insertBefore(img, art.firstChild.nextSibling)
    })
  }

  function stripExtension(name) {
    var i = name.lastIndexOf('.')
    return i > 0 ? name.slice(0, i) : name
  }

  /* --- Details ------------------------------------------------------------ */

  var detailsItem = null

  function openDetails(id) {
    return api('GET', '/api/tv/items/' + encodeURIComponent(id)).then(drawDetails, handleError)
  }

  function drawDetails(item) {
    detailsItem = item
    var film = item.movie || {}

    $('details-title').textContent = (film.title || stripExtension(item.name)) + (film.year ? '  (' + film.year + ')' : '')

    var meta = $('details-meta')
    meta.textContent = ''
    var parts = []
    if (film.runtime) parts.push(Math.floor(film.runtime / 60) + 'h ' + (film.runtime % 60) + 'm')
    if (film.genres && film.genres.length) parts.push(film.genres.slice(0, 3).join(', '))
    if (film.rating) {
      var r = document.createElement('span')
      r.className = 'rating'
      r.textContent = '★ ' + film.rating.toFixed(1)
      meta.appendChild(r)
      if (parts.length) meta.appendChild(document.createTextNode('   ·   '))
    }
    meta.appendChild(document.createTextNode(parts.join('   ·   ')))

    $('details-tagline').textContent = film.tagline || ''
    $('details-tagline').hidden = !film.tagline
    $('details-overview').textContent = film.overview ||
      (item.movie ? '' : 'SAND has no film details for this one yet. Turn on film lookup for its folder on your computer to fetch a poster and a synopsis.')

    var credits = $('details-credits')
    credits.textContent = ''
    if (film.directors && film.directors.length) addCredit(credits, 'Directed by', film.directors.join(', '))
    if (film.cast && film.cast.length) {
      addCredit(credits, 'Starring', film.cast.slice(0, 5).map(function (c) { return c.name }).join(', '))
    }

    $('details-file').textContent = item.name + '  ·  ' + formatBytes(item.size)

    var poster = $('details-poster')
    var backdrop = $('details-backdrop')
    poster.textContent = '🎬'
    backdrop.style.backgroundImage = ''
    if (item.poster) {
      posterFor(item.id).then(function (url) {
        if (!url || detailsItem !== item) return
        poster.textContent = ''
        var img = document.createElement('img')
        img.alt = ''
        img.src = url
        poster.appendChild(img)
        backdrop.style.backgroundImage = 'url("' + url + '")'
      })
    }

    var buttons = $('details-buttons')
    buttons.textContent = ''
    var progress = progressOf(item.id)
    var first
    if (progress && progress.pos && !progress.watched) {
      first = addButton(buttons, '▶  Resume from ' + formatTime(progress.pos), true, function () { play(item, progress.pos) })
      addButton(buttons, 'Play from the start', false, function () { play(item, 0) })
    } else {
      first = addButton(buttons, '▶  Play', true, function () { play(item, 0) })
    }
    if (progress && progress.watched) {
      addButton(buttons, 'Mark as not watched', false, function () {
        save('sandtv.pos.' + item.id, null)
        drawDetails(item)
      })
    }
    addButton(buttons, 'Back', false, goBack)

    show('details', first)
    // Measured once the screen is showing, since a hidden box has no height.
    var scroller = $('details-scroll')
    scroller.scrollTop = 0
    setTimeout(function () { fitScroller(scroller) }, 0)
  }

  /* The synopsis box is only something to move to when it has more than it
     shows. When it does, it joins the D-pad's targets — Up from the buttons
     lands on it, and Up and Down then scroll it — and says so underneath. */
  function fitScroller(el) {
    var overflows = el.scrollHeight > el.clientHeight + 2
    if (overflows) el.setAttribute('data-focus', '')
    else el.removeAttribute('data-focus')
    markScroller(el)
  }

  function markScroller(el) {
    var overflows = el.scrollHeight > el.clientHeight + 2
    var above = overflows && el.scrollTop > 2
    var below = overflows && el.scrollTop + el.clientHeight < el.scrollHeight - 2
    el.classList.toggle('more-above', above)
    el.classList.toggle('more-below', below)
    var hint = $('details-more')
    hint.hidden = !overflows
    if (overflows) {
      hint.textContent = document.activeElement === el
        ? (below ? '▼ more   ·   ▲▼ to read' : '▼ to the buttons')
        : '▲ press up to read the rest'
    }
  }

  /* Up and Down inside a box with more to show scroll it, a few lines at a
     time; at either end they move on, as anywhere else. */
  function scrollWithin(el, dir) {
    var step = Math.max(40, el.clientHeight * 0.6)
    if (dir === 'down' && el.scrollTop + el.clientHeight < el.scrollHeight - 2) {
      el.scrollTop += step
      return true
    }
    if (dir === 'up' && el.scrollTop > 2) {
      el.scrollTop -= step
      return true
    }
    return false
  }

  function addCredit(dl, label, value) {
    var dt = document.createElement('dt')
    dt.textContent = label
    var dd = document.createElement('dd')
    dd.textContent = value
    dl.appendChild(dt)
    dl.appendChild(dd)
  }

  function addButton(parent, label, primary, onClick) {
    var b = document.createElement('button')
    b.className = 'btn' + (primary ? ' btn-primary' : '')
    b.setAttribute('data-focus', '')
    b.textContent = label
    b.addEventListener('click', onClick)
    parent.appendChild(b)
    return b
  }

  /* --- Playing ------------------------------------------------------------ */

  var playingId = null

  function play(item, startMs) {
    var title = (item.movie && item.movie.title) || stripExtension(item.name)
    api('POST', '/api/tv/items/' + encodeURIComponent(item.id) + '/play').then(function (link) {
      var url = new URL(link.url, window.location.href).href
      var subtitles = (link.subtitles || []).map(function (s) {
        return { url: new URL(s.url, window.location.href).href, label: s.label, language: s.language || '', mime: s.mime }
      })
      playingId = item.id
      // playWith carries the subtitles; an app from before it existed only
      // has play, and gets the film without them.
      if (native && native.playWith) {
        native.playWith(JSON.stringify({
          url: url, title: title, startMs: Math.floor(startMs || 0), itemId: item.id, subtitles: subtitles,
        }))
        return
      }
      if (native && native.play) {
        native.play(url, title, Math.floor(startMs || 0), item.id)
        return
      }
      playInBrowser(url, title, startMs || 0, subtitles)
    }, handleError)
  }

  /* The native player reports where it was stopped. */
  function onPlaybackStopped(id, posMs, durMs) {
    setProgress(id, posMs, durMs)
    playingId = null
    if (current === 'details' && detailsItem && detailsItem.id === id) drawDetails(detailsItem)
  }

  var video = $('player')
  var barTimer = null

  function playInBrowser(url, title, startMs, subtitles) {
    $('player-title').textContent = title
    while (video.firstChild) video.removeChild(video.firstChild)
    // A browser reads WebVTT and nothing else in a <track>.
    ;(subtitles || []).forEach(function (s) {
      if (s.mime !== 'text/vtt') return
      var track = document.createElement('track')
      track.kind = 'subtitles'
      track.label = s.label
      if (s.language) track.srclang = s.language
      track.src = s.url
      video.appendChild(track)
    })
    video.src = url
    // A seek before the length is known is ignored, so it waits for it.
    if (startMs) {
      video.addEventListener('loadedmetadata', function seek() {
        video.removeEventListener('loadedmetadata', seek)
        video.currentTime = startMs / 1000
      })
    }
    show('player', null)
    video.focus()
    var attempt = video.play()
    if (attempt && attempt.catch) {
      attempt.catch(function () {
        toast('This browser cannot play this file. The SAND TV app for Fire TV plays far more formats.')
      })
    }
    wakeBar()
  }

  function stopBrowserPlayer() {
    if (playingId) setProgress(playingId, video.currentTime * 1000, (video.duration || 0) * 1000)
    var id = playingId
    playingId = null
    video.pause()
    video.removeAttribute('src')
    video.load()
    if (detailsItem && detailsItem.id === id) drawDetails(detailsItem)
    else show('library')
  }

  function wakeBar() {
    var bar = $('player-bar')
    bar.classList.remove('idle')
    clearTimeout(barTimer)
    barTimer = setTimeout(function () { if (!video.paused) bar.classList.add('idle') }, 3500)
  }

  video.addEventListener('timeupdate', function () {
    var d = video.duration || 0
    $('player-fill').style.width = d ? (video.currentTime / d * 100) + '%' : '0'
    $('player-time').textContent = formatTime(video.currentTime * 1000) + (d ? ' / ' + formatTime(d * 1000) : '')
  })
  video.addEventListener('ended', function () {
    if (playingId) setProgress(playingId, video.duration * 1000, video.duration * 1000)
    stopBrowserPlayer()
  })
  video.addEventListener('error', function () {
    toast('This browser cannot play this file. The SAND TV app for Fire TV plays far more formats.')
  })

  function playerKey(key) {
    wakeBar()
    if (key === 'Enter' || key === ' ' || key === 'MediaPlayPause') {
      if (video.paused) video.play(); else video.pause()
      return true
    }
    if (key === 'ArrowRight' || key === 'MediaFastForward') { video.currentTime += 10; return true }
    if (key === 'ArrowLeft' || key === 'MediaRewind') { video.currentTime = Math.max(0, video.currentTime - 10); return true }
    if (key === 'ArrowUp') { video.currentTime += 60; return true }
    if (key === 'ArrowDown') { video.currentTime = Math.max(0, video.currentTime - 60); return true }
    return false
  }

  /* --- Menu --------------------------------------------------------------- */

  function openMenu() {
    api('GET', '/api/tv/session').then(function (s) {
      $('menu-title').textContent = s.label
      $('menu-expiry').textContent = 'This TV can watch ' + s.label + ' until ' + formatDate(s.expires_at) + '.'
      show('menu')
    }, handleError)
  }

  function signOut() {
    api('POST', '/api/tv/logout').then(null, function () {}).then(function () {
      unpairLocally()
      showPair()
    })
  }

  /* --- Back --------------------------------------------------------------- */

  /* Returns true if Back was handled here; false means the app should take it
     (and leave), which is what Back on the first screen means on a TV. */
  function goBack() {
    switch (current) {
      case 'player':
        stopBrowserPlayer()
        return true
      case 'details':
        openLibrary(library.path, library.all, detailsItem && detailsItem.id)
        return true
      case 'menu':
        show('library', lastFocus.library)
        return true
      case 'library':
        if (library.all) { openLibrary('/', false); return true }
        if (library.path !== '/') { openLibrary(library.parent || '/', false); return true }
        return false
      case 'pair':
        if (typed.length) { deleteChar(); return true }
        return false
      default:
        return false
    }
  }

  /* --- Keys --------------------------------------------------------------- */

  /* Older engines report keyCode only; Fire TV remotes send these. */
  var KEYCODES = {
    37: 'ArrowLeft', 38: 'ArrowUp', 39: 'ArrowRight', 40: 'ArrowDown',
    13: 'Enter', 27: 'Escape', 8: 'Backspace', 179: 'MediaPlayPause',
    85: 'MediaPlayPause', 227: 'MediaRewind', 228: 'MediaFastForward',
  }

  document.addEventListener('keydown', function (e) {
    var key = e.key && e.key !== 'Unidentified' ? e.key : KEYCODES[e.keyCode]
    if (!key) return

    if (current === 'player' && playerKey(key)) { e.preventDefault(); return }

    if (key === 'Escape' || key === 'Backspace' || key === 'GoBack' || key === 'BrowserBack') {
      if (current === 'pair' && key === 'Backspace') { deleteChar(); e.preventDefault(); return }
      if (goBack()) e.preventDefault()
      return
    }

    if (current === 'pair' && key.length === 1) {
      var c = key.toUpperCase()
      if (CODE_ALPHABET.indexOf(c) >= 0) { typeChar(c); e.preventDefault(); return }
    }

    var dir = { ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'up', ArrowDown: 'down' }[key]
    if (!dir) return
    e.preventDefault()
    var active = document.activeElement
    if (active && active.classList && active.classList.contains('scroller') && scrollWithin(active, dir)) return
    move(dir)
  })

  document.addEventListener('mousemove', function () { if (current === 'player') wakeBar() })

  /* --- Formatting --------------------------------------------------------- */

  function formatTime(ms) {
    var s = Math.max(0, Math.floor(ms / 1000))
    var h = Math.floor(s / 3600)
    var m = Math.floor((s % 3600) / 60)
    var sec = s % 60
    var mm = (h ? String(m).padStart(2, '0') : String(m))
    return (h ? h + ':' : '') + mm + ':' + String(sec).padStart(2, '0')
  }

  function formatBytes(n) {
    if (!n) return '0 B'
    var units = ['B', 'KB', 'MB', 'GB', 'TB']
    var i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)))
    return (n / Math.pow(1024, i)).toFixed(i ? 1 : 0) + ' ' + units[i]
  }

  function formatDate(iso) {
    var d = new Date(iso)
    try {
      return d.toLocaleString(undefined, { weekday: 'short', day: 'numeric', month: 'short', hour: 'numeric', minute: '2-digit' })
    } catch (e) {
      return d.toString()
    }
  }

  /* --- Start ---------------------------------------------------------------- */

  buildKeypad()
  ;(function () {
    var scroller = $('details-scroll')
    var mark = function () { markScroller(scroller) }
    scroller.addEventListener('scroll', mark)
    scroller.addEventListener('focus', mark)
    scroller.addEventListener('blur', mark)
    window.addEventListener('resize', function () { if (current === 'details') fitScroller(scroller) })
  })()
  $('pair-delete').addEventListener('click', deleteChar)
  $('pair-connect').addEventListener('click', connect)
  $('locked-retry').addEventListener('click', checkUnlocked)
  $('locked-unpair').addEventListener('click', signOut)
  $('tab-folders').addEventListener('click', function () { openLibrary('/', false) })
  $('tab-all').addEventListener('click', function () { openLibrary('/', true) })
  $('tab-menu').addEventListener('click', openMenu)
  $('menu-back').addEventListener('click', function () { show('library', lastFocus.library) })
  $('menu-unpair').addEventListener('click', signOut)

  if (native && native.changeServer) {
    $('pair-server').hidden = false
    $('menu-server').hidden = false
    var change = function () { native.changeServer() }
    $('pair-server').addEventListener('click', change)
    $('menu-server').addEventListener('click', change)
  }

  window.sandTV = { back: goBack, onPlaybackStopped: onPlaybackStopped }

  if (!token) {
    showPair()
  } else {
    api('GET', '/api/tv/session').then(function (s) {
      if (s.locked) showLocked()
      else openLibrary('/', false)
    }, handleError)
  }
})()
