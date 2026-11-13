package server

import (
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/chinmay28/SandHarbor/internal/netif"
)

// The server's side of the network, for the TV's Stats panel.
//
// The TV can say how it is connected — Wi-Fi or Ethernet, the link's rate,
// its signal — and how fast a film is arriving, but not whether the far end is
// the slow one. The far end is this machine, so the server says: the link of
// the network interface the TV's request came in on, which is the interface
// the film goes out through, and how fast that interface is sending, the film
// and everything else together. Set beside the TV's own figures, that tells a
// server on a slow link, or a full one, from a TV that is slow to receive.
//
// The TV asks only while its Stats panel is open, every few seconds. However
// many ask, the interface is read at most once every streamNetworkEvery, and
// everyone in between is given the last reading.

// streamNetworkEvery is the most often the interface is read.
const streamNetworkEvery = 2 * time.Second

// streamNetworkResponse is what the TV is told. What the system does not say
// is left out rather than sent as zero.
type streamNetworkResponse struct {
	Interface    string     `json:"interface,omitempty"`
	Kind         netif.Kind `json:"kind"`
	LinkMbps     int        `json:"link_mbps,omitempty"`
	FrequencyMHz int        `json:"frequency_mhz,omitempty"`
	SignalDBm    int        `json:"signal_dbm,omitempty"`

	// SendBPS is how fast the interface is sending, in bits a second. It is
	// absent until two readings make a rate, so from the second question on.
	SendBPS *int64 `json:"send_bps,omitempty"`

	SampledAt time.Time `json:"sampled_at"`
}

// handleStreamNetwork says how this machine is connected to the network a
// stream goes out over.
//
// The stream's ticket is the credential, as it is for the film itself: the
// player has nothing else to offer, and a ticket that would no longer play —
// expired, voided by a lock, or minted under a TV code that has since been
// revoked — is refused the same way. Nothing is read from the vault, and
// asking is not use of it.
func (s *Server) handleStreamNetwork(w http.ResponseWriter, r *http.Request) {
	ticket, ok := s.streams.lookup(r.PathValue("token"))
	if !ok || (ticket.grant != "" && !s.tv.Live(ticket.grant)) {
		writeError(w, http.StatusNotFound, "this stream link has expired", "NO_TICKET")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")

	now := time.Now()
	out := streamNetworkResponse{Kind: netif.Unknown, SampledAt: now}
	if local, ok := localAddress(r); ok {
		if sample, err := s.links.Sample(local, now); err == nil {
			out = networkResponse(sample)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func networkResponse(sample netif.Sample) streamNetworkResponse {
	out := streamNetworkResponse{
		Interface:    sample.Name,
		Kind:         sample.Kind,
		LinkMbps:     sample.Mbps,
		FrequencyMHz: sample.FrequencyMHz,
		SignalDBm:    sample.SignalDBm,
		SampledAt:    sample.At,
	}
	if sample.Rated {
		bps := sample.SendBitsPerSecond
		out.SendBPS = &bps
	}
	return out
}

// localAddress is the address of this machine that a request came in on.
// Behind a reverse proxy it is the address the proxy reached the server on,
// which is the proxy's link and not the TV's.
func localAddress(r *http.Request) (netip.Addr, bool) {
	tcp, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	return tcp.AddrPort().Addr().Unmap(), true
}
