/* The app's connect page: the servers discovery found, or an address typed
   by hand. window.SandTVNative is the app (MainActivity.Bridge); it answers
   through window.sandConnect. */
(function () {
  'use strict'
  var nativeApp = window.SandTVNative
  function $(id) { return document.getElementById(id) }

  function setError(msg) {
    $('error').textContent = msg || ''
    $('error').hidden = !msg
  }

  function search() {
    setError('')
    $('status').textContent = 'Looking on this network…'
    $('servers').textContent = ''
    if (nativeApp) nativeApp.discover()
  }

  function connect(address) {
    setError('')
    $('status').textContent = 'Connecting to ' + address + '…'
    if (nativeApp) nativeApp.connect(address)
  }

  window.sandConnect = {
    onDiscovered: function (servers) {
      var list = $('servers')
      list.textContent = ''
      if (!servers.length) {
        $('status').textContent = 'No SAND Vault answered on this network. Is it running, with the TV on the same Wi-Fi? You can also type its address.'
        $('manual').focus()
        return
      }
      $('status').textContent = servers.length === 1 ? 'Found one:' : 'Found ' + servers.length + ':'
      servers.forEach(function (s) {
        var b = document.createElement('button')
        b.className = 'server'
        b.setAttribute('data-focus', '')
        var name = document.createElement('span')
        name.textContent = s.name
        var addr = document.createElement('small')
        addr.textContent = s.origin.replace(/^http:\/\//, '') + (s.version ? '  ·  ' + s.version : '')
        b.appendChild(name)
        b.appendChild(addr)
        b.addEventListener('click', function () { connect(s.origin) })
        list.appendChild(b)
      })
      list.firstChild.focus()
    },
    onConnectFailed: function (msg) {
      $('status').textContent = ''
      setError(msg)
    },
  }

  /* Back on this page: close the address form if it is open, else leave. */
  window.sandTV = {
    back: function () {
      if (!$('manual-form').hidden) {
        $('manual-form').hidden = true
        $('manual').focus()
        return true
      }
      return false
    },
  }

  /* Up and down walk the buttons in order; left and right too, so the row
     of two works the way it looks. */
  document.addEventListener('keydown', function (e) {
    var dirs = { ArrowUp: -1, ArrowLeft: -1, ArrowDown: 1, ArrowRight: 1 }
    var step = dirs[e.key]
    if (!step) return
    if (document.activeElement === $('address') && (e.key === 'ArrowLeft' || e.key === 'ArrowRight')) return
    var all = Array.prototype.filter.call(document.querySelectorAll('[data-focus]'), function (el) {
      return el.offsetParent !== null
    })
    var at = all.indexOf(document.activeElement)
    var next = all[Math.max(0, Math.min(all.length - 1, at + step))]
    if (next) { next.focus(); e.preventDefault() }
  })

  $('search').addEventListener('click', search)
  $('manual').addEventListener('click', function () {
    $('manual-form').hidden = false
    var saved = nativeApp && nativeApp.savedServer ? nativeApp.savedServer() : ''
    if (saved && !$('address').value) $('address').value = saved.replace(/^http:\/\//, '')
    $('address').focus()
  })
  $('manual-form').addEventListener('submit', function (e) {
    e.preventDefault()
    var value = $('address').value.trim()
    if (value) connect(value)
  })

  var hash = decodeURIComponent((location.hash.match(/error=([^&]*)/) || [])[1] || '')
  if (hash) setError(hash)
  search()
})()
