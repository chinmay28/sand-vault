package tvdiscover

import (
	"context"
	"net"
	"testing"
	"time"
)

// serve starts a responder on a loopback port and returns its address.
func serve(t *testing.T, announce func() Announcement) net.Addr {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, conn, announce) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v after its context ended, want nil", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not stop when its context ended")
		}
	})
	return conn.LocalAddr()
}

func client(t *testing.T) net.PacketConn {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestProbeFindsTheServer(t *testing.T) {
	addr := serve(t, func() Announcement {
		return Announcement{Name: "nas", Port: 8123, Version: "v2026.11.1", Service: "something else"}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	found, err := Probe(ctx, client(t), addr)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d servers, want 1: %+v", len(found), found)
	}
	got := found[0]
	if got.Host != "127.0.0.1" || got.Port != 8123 || got.Name != "nas" || got.Version != "v2026.11.1" {
		t.Errorf("found %+v", got)
	}
	// The responder stamps the service itself, whatever announce said.
	if got.Service != Service {
		t.Errorf("service = %q, want %q", got.Service, Service)
	}
}

func TestOnlyTheExactQueryIsAnswered(t *testing.T) {
	addr := serve(t, func() Announcement { return Announcement{Name: "nas", Port: 8123} })
	conn := client(t)

	for _, junk := range []string{"", "hello", "SAND-TV-DISCOVER/2", Query + "x"} {
		if _, err := conn.WriteTo([]byte(junk), addr); err != nil {
			t.Fatal(err)
		}
	}
	conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 512)
	if n, _, err := conn.ReadFrom(buf); err == nil {
		t.Fatalf("answered a packet that was not the query: %q", buf[:n])
	}
}

func TestProbeIgnoresAnswersThatAreNotSAND(t *testing.T) {
	// Something else on the port answers with JSON of the wrong shape.
	impostor, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer impostor.Close()
	go func() {
		buf := make([]byte, 64)
		_, from, err := impostor.ReadFrom(buf)
		if err != nil {
			return
		}
		impostor.WriteTo([]byte(`{"service":"printer","port":631}`), from)
		impostor.WriteTo([]byte(`not json`), from)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	found, err := Probe(ctx, client(t), impostor.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("found %+v, want nothing", found)
	}
}

func TestAnswerable(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1":      true,
		"192.168.1.40":   true,
		"10.1.2.3":       true,
		"172.20.0.9":     true,
		"169.254.10.10":  true,
		"100.101.102.10": true,
		"8.8.8.8":        false,
		"203.0.113.7":    false,
	} {
		if got := answerable(&net.UDPAddr{IP: net.ParseIP(ip), Port: 5000}); got != want {
			t.Errorf("answerable(%s) = %v, want %v", ip, got, want)
		}
	}
	if answerable(&net.TCPAddr{IP: net.ParseIP("127.0.0.1")}) {
		t.Error("answered something that is not a UDP address")
	}
}
