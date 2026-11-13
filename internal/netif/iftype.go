package netif

import "math"

// What Windows says about an adapter, turned into this package's terms. Kept
// apart from the calls that ask, and built everywhere, so that it is tested
// everywhere.

// kindOfIfType is what an adapter's IANA interface type says it is.
func kindOfIfType(ifType uint32) Kind {
	switch ifType {
	case 6: // IF_TYPE_ETHERNET_CSMACD
		return Ethernet
	case 71: // IF_TYPE_IEEE80211
		return WiFi
	case 24: // IF_TYPE_SOFTWARE_LOOPBACK
		return Loopback
	case 23, 53, 131: // IF_TYPE_PPP, IF_TYPE_PROP_VIRTUAL, IF_TYPE_TUNNEL
		return Virtual
	}
	return Unknown
}

// mbpsOf turns an adapter's link speed in bits a second into megabits. All
// ones is how an adapter says it does not know.
func mbpsOf(bitsPerSecond uint64) int {
	if bitsPerSecond == math.MaxUint64 {
		return 0
	}
	return int(bitsPerSecond / 1_000_000)
}
