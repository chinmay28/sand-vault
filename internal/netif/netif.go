// Package netif says how this machine is connected to its network: for the
// interface that holds an address, whether it is Ethernet or Wi-Fi, the rate
// its link runs at, and how much it has sent.
//
// Everything here is what the operating system reports, read when asked.
// Nothing is measured: a link's rate is what the two ends of it agreed on, not
// what the cable or the air will carry, and on Wi-Fi a good deal less gets
// through than the rate says.
package netif

import (
	"errors"
	"net"
	"net/netip"
)

// Kind is what an interface is.
type Kind string

const (
	// Unknown is an interface the system does not describe, or one this
	// package does not know how to ask about on this system.
	Unknown Kind = "unknown"
	// Ethernet is a wired link.
	Ethernet Kind = "ethernet"
	// WiFi is a wireless one.
	WiFi Kind = "wifi"
	// Virtual is an interface with no hardware of its own — a container's, a
	// VPN's, a bridge over several ports — whose rate, if it gives one, is
	// not a wire's.
	Virtual Kind = "virtual"
	// Loopback is the machine talking to itself.
	Loopback Kind = "loopback"
)

// Link is one interface's link, as the operating system reports it.
type Link struct {
	// Name is the interface's name: enp5s0, wlan0, "Ethernet 2".
	Name string
	Kind Kind

	// Mbps is the rate the link runs at, in megabits a second: what an
	// Ethernet link negotiated, or what a Wi-Fi link is sending at just now.
	// Zero when the system does not say.
	Mbps int

	// FrequencyMHz is a Wi-Fi link's channel, and SignalDBm its strength.
	// Zero when not known, or not Wi-Fi.
	FrequencyMHz int
	SignalDBm    int

	// SentBytes is how much the interface has sent since it came up, which
	// is what a send rate is worked out from. Counted is false when the
	// system keeps no such count.
	SentBytes uint64
	Counted   bool
}

// ErrNoInterface is what ForAddress returns for an address no interface on
// this machine holds.
var ErrNoInterface = errors.New("netif: no interface holds that address")

// ForAddress reports the link of the interface holding ip. For a server, ip
// is the local address a connection came in on, and that interface is the
// one the connection's traffic goes back out through.
func ForAddress(ip netip.Addr) (Link, error) {
	iface, err := holding(ip)
	if err != nil {
		return Link{}, err
	}
	if iface.Flags&net.FlagLoopback != 0 {
		return Link{Name: iface.Name, Kind: Loopback}, nil
	}
	return describe(iface), nil
}

// holding finds the interface an address is assigned to. A v4 address seen
// through a dual-stack socket arrives as ::ffff:a.b.c.d, and a link-local v6
// one with a zone; both are compared as the plain address.
func holding(ip netip.Addr) (net.Interface, error) {
	want := ip.Unmap().WithZone("")
	if !want.IsValid() || want.IsUnspecified() {
		return net.Interface{}, ErrNoInterface
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return net.Interface{}, err
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if assigned(a) == want {
				return iface, nil
			}
		}
	}
	return net.Interface{}, ErrNoInterface
}

// assigned is the address in one of an interface's addresses.
func assigned(a net.Addr) netip.Addr {
	var raw net.IP
	switch a := a.(type) {
	case *net.IPNet:
		raw = a.IP
	case *net.IPAddr:
		raw = a.IP
	}
	ip, _ := netip.AddrFromSlice(raw)
	return ip.Unmap()
}
