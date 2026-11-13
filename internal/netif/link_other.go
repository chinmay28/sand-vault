//go:build !linux && !windows

package netif

import "net"

// Elsewhere — macOS, the BSDs — the interface is found, but its link is not
// read: it is reported by name, as Unknown.
func describe(iface net.Interface) Link {
	return Link{Name: iface.Name, Kind: Unknown}
}
