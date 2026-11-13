//go:build windows

package netif

import (
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On Windows an adapter's type and link rate come from GetAdaptersAddresses —
// for Wi-Fi, the rate it is sending at, which Windows keeps current — and the
// count of bytes it has sent from GetIfEntry2Ex. A Wi-Fi link's band and
// signal would take the WLAN API, and are not reported.

func describe(iface net.Interface) Link {
	link := Link{Name: iface.Name, Kind: Unknown}
	list, err := adapters()
	if err != nil {
		return link
	}
	for a := list; a != nil; a = a.Next {
		// Go's net package numbers an interface by its v4 index, or its v6
		// one when it has no v4.
		if int(a.IfIndex) != iface.Index && int(a.Ipv6IfIndex) != iface.Index {
			continue
		}
		link.Kind = kindOfIfType(a.IfType)
		link.Mbps = mbpsOf(a.TransmitLinkSpeed)
		row := windows.MibIfRow2{InterfaceLuid: a.Luid}
		if windows.GetIfEntry2Ex(windows.MibIfEntryNormal, &row) == nil {
			link.SentBytes, link.Counted = row.OutOctets, true
		}
		break
	}
	return link
}

// adapters lists the machine's adapters, asked the way Go's own net package
// asks: with a buffer that grows until the list fits.
func adapters() (*windows.IpAdapterAddresses, error) {
	const flags = windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER
	size := uint32(15_000)
	for range 3 {
		buf := make([]byte, size)
		list := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, list, &size)
		if err == nil {
			return list, nil
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return nil, err
		}
	}
	return nil, windows.ERROR_BUFFER_OVERFLOW
}
