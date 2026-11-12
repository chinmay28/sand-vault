// Package tvdiscover lets a TV on the same network find SAND without being
// told its address.
//
// Typing "192.168.1.20:8123" with a D-pad is the step that would make pairing a
// TV a chore, so the app shouts instead: it broadcasts a fixed query on a UDP
// port, and every SAND server listening answers with what the app needs to
// build an address — the port its web server is on, a name to show in a list,
// and its version. The address itself is wherever the answer came from.
//
// The answer gives away nothing a port scan would not: that SAND is here, and
// where. It authorizes nothing — pairing still needs a code the owner minted.
//
// Two limits keep it from being a nuisance. It answers only the exact query, so
// stray packets are ignored rather than echoed; and it answers only addresses
// on a private, loopback or link-local network, so a server bound to a public
// address cannot be used to reflect traffic at a stranger.
package tvdiscover

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the UDP port the responder listens on — the one after the web
// server's default, so the pair are easy to open in a firewall together.
const DefaultPort = 8124

// Query is what a TV broadcasts. Versioned, so a later query that wants a
// different answer can be told apart from this one.
const Query = "SAND-TV-DISCOVER/1"

// Service is what every answer says it is, so a client can tell SAND's answer
// from anything else that happens to be on the port.
const Service = "sand-vault"

// Announcement is the answer.
type Announcement struct {
	Service string `json:"service"`

	// Name is what the TV lists this server as — the machine's hostname.
	Name string `json:"name"`

	// Port is the web server's port. The scheme is http unless Secure.
	Port    int    `json:"port"`
	Secure  bool   `json:"secure,omitempty"`
	Version string `json:"version,omitempty"`
}

// Found is one answer to a probe, with the address it came from.
type Found struct {
	Announcement
	Host string `json:"host"`
}

// Serve answers queries arriving on conn until ctx is done, then closes conn.
// announce is asked for the answer each time, so a value that changes — the
// version after an upgrade in place, say — is never stale.
func Serve(ctx context.Context, conn net.PacketConn, announce func() Announcement) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	buf := make([]byte, 512)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// A transient read error — an ICMP unreachable surfacing on a
			// later read, say — is not a reason to stop answering.
			continue
		}
		if strings.TrimSpace(string(buf[:n])) != Query || !answerable(from) {
			continue
		}
		reply := announce()
		reply.Service = Service
		body, err := json.Marshal(reply)
		if err != nil {
			continue
		}
		conn.WriteTo(body, from)
	}
}

// Listen opens the UDP port and serves it until ctx is done. addr is a
// host:port; an empty host listens on every interface, which is what receiving
// a broadcast needs.
func Listen(ctx context.Context, addr string, announce func() Announcement) error {
	conn, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return err
	}
	return Serve(ctx, conn, announce)
}

// answerable reports whether a query came from somewhere worth answering: the
// local network, never the internet.
func answerable(from net.Addr) bool {
	udp, ok := from.(*net.UDPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(udp.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		// Carrier-grade NAT space, which is what Tailscale hands out.
		netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

// Probe sends the query to target and collects answers until ctx is done. It
// is what the TV app does, written here so the round trip can be tested and so
// `sand` itself could list the servers it can see.
func Probe(ctx context.Context, conn net.PacketConn, target net.Addr) ([]Found, error) {
	if _, err := conn.WriteTo([]byte(Query), target); err != nil {
		return nil, err
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Second)
	}
	conn.SetReadDeadline(deadline)

	var found []Found
	seen := map[string]bool{}
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				return found, nil
			}
			return found, err
		}
		var a Announcement
		if json.Unmarshal(buf[:n], &a) != nil || a.Service != Service || a.Port <= 0 {
			continue
		}
		host := from.String()
		if udp, ok := from.(*net.UDPAddr); ok {
			host = udp.IP.String()
		}
		key := net.JoinHostPort(host, strconv.Itoa(a.Port))
		if seen[key] {
			continue
		}
		seen[key] = true
		found = append(found, Found{Announcement: a, Host: host})
	}
}
