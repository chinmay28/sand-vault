package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/chinmay28/SandHarbor/internal/netif"
)

// fakeNIC stands in for the machine's network interface.
type fakeNIC struct {
	mu    sync.Mutex
	link  netif.Link
	err   error
	asked []netip.Addr
}

func (f *fakeNIC) read(addr netip.Addr) (netif.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, addr)
	return f.link, f.err
}

// tvTicket sets up a TV playing a film, and returns the ticket its stream
// link carries and the code it was minted under.
func tvTicket(t *testing.T) (c *testClient, ticket, code string) {
	t.Helper()
	c, films := tvLibrary(t)
	code = c.mintTVCode("/films", 60)
	device := c.pairTV(code)
	w, link := c.tvDo(http.MethodPost, "/api/tv/items/"+films["thing"]+"/play", device, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("play: %d %v", w.Code, link)
	}
	// /stream/{ticket}/{name}
	parts := strings.Split(strings.SplitN(link["url"].(string), "?", 2)[0], "/")
	if len(parts) < 3 || parts[1] != "stream" {
		t.Fatalf("stream link %v is not /stream/{ticket}/...", link["url"])
	}
	return c, parts[2], code
}

// askNetwork asks the way the TV does, on a connection that came in on local
// (nil for a request that carries no local address at all).
func (c *testClient) askNetwork(ticket string, local *net.TCPAddr) (*httptest.ResponseRecorder, map[string]any) {
	c.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+ticket+"/network", nil)
	req.Host = "example.test"
	req.RemoteAddr = "192.168.1.50:41000"
	if local != nil {
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))
	}
	w := httptest.NewRecorder()
	c.handler.ServeHTTP(w, req)
	out := map[string]any{}
	json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

var tvSide = &net.TCPAddr{IP: net.ParseIP("::ffff:192.168.1.10"), Port: 8123}

func TestTheServerSaysHowItIsConnected(t *testing.T) {
	c, ticket, _ := tvTicket(t)
	nic := &fakeNIC{link: netif.Link{Name: "enp5s0", Kind: netif.Ethernet, Mbps: 1000, SentBytes: 1000, Counted: true}}
	c.server.links = netif.NewSampler(nic.read, 0)

	w, body := c.askNetwork(ticket, tvSide)
	if w.Code != http.StatusOK {
		t.Fatalf("network: %d %v", w.Code, body)
	}
	if body["kind"] != "ethernet" || body["link_mbps"] != float64(1000) || body["interface"] != "enp5s0" {
		t.Errorf("body = %v, want enp5s0's 1000 Mb/s Ethernet link", body)
	}
	for _, absent := range []string{"frequency_mhz", "signal_dbm", "send_bps"} {
		if _, ok := body[absent]; ok {
			t.Errorf("%s = %v on the first answer of an Ethernet link, want it left out", absent, body[absent])
		}
	}
	if _, ok := body["sampled_at"].(string); !ok {
		t.Errorf("sampled_at = %v", body["sampled_at"])
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	// The interface asked about is the one holding the address the request
	// came in on, as a plain v4 address rather than the socket's mapped one.
	if want := netip.MustParseAddr("192.168.1.10"); len(nic.asked) != 1 || nic.asked[0] != want {
		t.Errorf("asked about %v, want [%v]", nic.asked, want)
	}

	// The second answer has a send rate: the count moved between the two.
	nic.mu.Lock()
	nic.link.SentBytes += 5_000_000
	nic.mu.Unlock()
	_, body = c.askNetwork(ticket, tvSide)
	if bps, ok := body["send_bps"].(float64); !ok || bps <= 0 {
		t.Errorf("send_bps = %v on the second answer, want a rate", body["send_bps"])
	}
}

func TestTheServerSaysWiFiInFull(t *testing.T) {
	c, ticket, _ := tvTicket(t)
	nic := &fakeNIC{link: netif.Link{Name: "wlp4s0", Kind: netif.WiFi, Mbps: 72, FrequencyMHz: 2437, SignalDBm: -67}}
	c.server.links = netif.NewSampler(nic.read, 0)

	_, body := c.askNetwork(ticket, tvSide)
	if body["kind"] != "wifi" || body["link_mbps"] != float64(72) || body["frequency_mhz"] != float64(2437) || body["signal_dbm"] != float64(-67) {
		t.Errorf("body = %v", body)
	}
}

func TestTheServerSaysWhenItCannotTell(t *testing.T) {
	c, ticket, _ := tvTicket(t)
	nic := &fakeNIC{err: errors.New("no such interface")}
	c.server.links = netif.NewSampler(nic.read, 0)

	w, body := c.askNetwork(ticket, tvSide)
	if w.Code != http.StatusOK || body["kind"] != "unknown" {
		t.Errorf("a failed reading = %d %v, want 200 and kind unknown", w.Code, body)
	}
	if _, ok := body["interface"]; ok {
		t.Errorf("a failed reading named an interface: %v", body)
	}

	// A request that arrived some way that carries no local address cannot be
	// matched to an interface, and none is read.
	nic.asked = nil
	w, body = c.askNetwork(ticket, nil)
	if w.Code != http.StatusOK || body["kind"] != "unknown" || len(nic.asked) != 0 {
		t.Errorf("no local address = %d %v (read %d times), want unknown without a reading", w.Code, body, len(nic.asked))
	}
}

func TestOnlyALiveStreamTicketIsAnswered(t *testing.T) {
	c, ticket, code := tvTicket(t)
	nic := &fakeNIC{link: netif.Link{Name: "enp5s0", Kind: netif.Ethernet}}
	c.server.links = netif.NewSampler(nic.read, 0)

	if w, body := c.askNetwork("not-a-ticket", tvSide); w.Code != http.StatusNotFound || body["code"] != "NO_TICKET" {
		t.Errorf("a made-up ticket = %d %v, want 404 NO_TICKET", w.Code, body)
	}
	if w, _ := c.askNetwork(ticket, tvSide); w.Code != http.StatusOK {
		t.Fatalf("the TV's own ticket = %d, want 200", w.Code)
	}

	// Revoking the code stops the film, and with it the question about it.
	if w, _ := c.json(http.MethodDelete, "/api/tv/codes/"+code, nil); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w, body := c.askNetwork(ticket, tvSide); w.Code != http.StatusNotFound || body["code"] != "NO_TICKET" {
		t.Errorf("after the code is revoked = %d %v, want 404 NO_TICKET", w.Code, body)
	}
	if len(nic.asked) != 1 {
		t.Errorf("the interface was read %d times, want only for the live ticket", len(nic.asked))
	}
}

func TestTheServerReadsItsLinkAtMostEveryFewSeconds(t *testing.T) {
	c, ticket, _ := tvTicket(t)
	nic := &fakeNIC{link: netif.Link{Name: "enp5s0", Kind: netif.Ethernet, Mbps: 1000}}
	c.server.links = netif.NewSampler(nic.read, streamNetworkEvery)

	for range 10 {
		if w, _ := c.askNetwork(ticket, tvSide); w.Code != http.StatusOK {
			t.Fatalf("network: %d", w.Code)
		}
	}
	if len(nic.asked) != 1 {
		t.Errorf("ten questions in a row read the interface %d times, want once", len(nic.asked))
	}
}

// End to end, with nothing standing in: a real connection to a real listener
// on the loopback, which the server must find and say is the loopback.
func TestTheServerFindsTheInterfaceARealConnectionCameInOn(t *testing.T) {
	c, ticket, _ := tvTicket(t)
	live := httptest.NewServer(c.handler)
	defer live.Close()

	resp, err := http.Get(live.URL + "/api/stream/" + ticket + "/network")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || body["kind"] != "loopback" || body["interface"] == "" {
		t.Errorf("over the loopback = %d %v, want the loopback interface by name", resp.StatusCode, body)
	}
}
