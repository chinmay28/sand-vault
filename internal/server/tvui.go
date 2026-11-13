package server

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"

	"github.com/chinmay28/SandHarbor/internal/tvdiscover"
	"github.com/chinmay28/SandHarbor/internal/version"
)

// The TV's screen: a small page drawn for a remote rather than a mouse, served
// at /tv/.
//
// It is its own few files rather than a route in the main app, for two
// reasons. A TV is a slow machine running an old browser engine, and the main
// app is a few megabytes of React written for a desktop; and the main app is
// the owner's, with every setting in it, which is not a thing to load onto a
// screen whose whole permission is one folder of films. The page is plain
// JavaScript, embedded as it stands, with nothing to build.
//
// The Fire TV app is a frame around it — a WebView that opens this page on the
// server it found, and hands films to a native player — so the screen updates
// with the server rather than with the app. Any other TV with a browser can
// open the same address.

//go:embed all:tvui
var tvAssets embed.FS

// tvPageCSP is the policy the TV page runs under: its own scripts and styles
// and nothing else, posters as blobs fetched with the token, and films from
// this origin.
const tvPageCSP = "default-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; " +
	"style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"

// mountTVApp serves the TV page at /tv/.
func (s *Server) mountTVApp(mux *http.ServeMux) error {
	sub, err := fs.Sub(tvAssets, "tvui")
	if err != nil {
		return fmt.Errorf("failed to get the TV page: %w", err)
	}
	files := http.StripPrefix("/tv", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /tv", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tv/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /tv/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", tvPageCSP)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// Revalidated every load, like the main app's index.html, so a TV
		// picks up a new screen the first time it opens after an upgrade.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	return nil
}

// discoveryPort is the UDP port discovery answers on, or zero when it is off —
// asked to be, or bound where no TV could reach the web server.
func (s *Server) discoveryPort() int {
	if s.TVDiscoveryPort <= 0 || !warnsOnBind(s.Bind) {
		return 0
	}
	return s.TVDiscoveryPort
}

// discoveryLoop answers TVs looking for SAND for the life of the process. A
// port that will not open is a log line and nothing more: the TV can still be
// pointed at the server by hand.
func (s *Server) discoveryLoop(port int) {
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "SAND Vault"
	}
	webPort := s.Port
	if webPort == 0 {
		webPort = DefaultPort
	}
	announce := func() tvdiscover.Announcement {
		return tvdiscover.Announcement{Name: name, Port: webPort, Version: version.String()}
	}

	addr := net.JoinHostPort("", strconv.Itoa(port))
	log.Printf("answering TVs looking for SAND on udp %s", addr)
	if err := tvdiscover.Listen(context.Background(), addr, announce); err != nil {
		log.Printf("TV discovery is off: %v — point the TV at this server's address instead", err)
	}
}
