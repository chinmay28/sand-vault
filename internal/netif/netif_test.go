package netif

import (
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestForAddressFindsTheLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::ffff:127.0.0.1"} {
		link, err := ForAddress(netip.MustParseAddr(addr))
		if err != nil {
			t.Fatalf("ForAddress(%s): %v", addr, err)
		}
		if link.Kind != Loopback || link.Name == "" {
			t.Errorf("ForAddress(%s) = %+v, want the loopback interface by name", addr, link)
		}
	}
}

func TestForAddressRefusesAnAddressNobodyHolds(t *testing.T) {
	// 203.0.113.0/24 is set aside for documentation. Sandboxes do hand such
	// addresses out, so a machine that holds this one is skipped, not failed.
	nobodys := netip.MustParseAddr("203.0.113.77")
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if assigned(a) == nobodys {
			t.Skipf("this machine holds %v", nobodys)
		}
	}

	for _, addr := range []netip.Addr{nobodys, netip.IPv4Unspecified(), {}} {
		if _, err := ForAddress(addr); !errors.Is(err, ErrNoInterface) {
			t.Errorf("ForAddress(%v) error = %v, want ErrNoInterface", addr, err)
		}
	}
}

func TestWindowsAdapterTypes(t *testing.T) {
	for ifType, want := range map[uint32]Kind{6: Ethernet, 71: WiFi, 24: Loopback, 131: Virtual, 23: Virtual, 53: Virtual, 1: Unknown, 243: Unknown} {
		if got := kindOfIfType(ifType); got != want {
			t.Errorf("kindOfIfType(%d) = %q, want %q", ifType, got, want)
		}
	}
	for bits, want := range map[uint64]int{1_000_000_000: 1000, 866_700_000: 866, 999_999: 0, ^uint64(0): 0} {
		if got := mbpsOf(bits); got != want {
			t.Errorf("mbpsOf(%d) = %d, want %d", bits, got, want)
		}
	}
}
