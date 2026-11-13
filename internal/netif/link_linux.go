//go:build linux

package netif

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// On Linux an interface's link is in sysfs: what it is (a "wireless"
// directory or a "phy80211" link for Wi-Fi, a "device" link for hardware of
// any kind), the rate an Ethernet link negotiated, and the count of bytes it
// has sent. A Wi-Fi link's rate and channel are asked of its driver (see
// wext_linux.go), and its signal is in /proc/net/wireless.

func describe(iface net.Interface) Link {
	return system.describe(iface.Name)
}

// sysLinks reads links from a sysfs tree. It is a value so that tests can
// point it at a copy of one.
type sysLinks struct {
	// net is sysfs's directory of interfaces.
	net string
	// wireless is the kernel's table of Wi-Fi signal strengths.
	wireless string
	// radio asks a Wi-Fi interface's driver its rate and channel.
	radio func(name string) (mbps, mhz int)
}

var system = sysLinks{net: "/sys/class/net", wireless: "/proc/net/wireless", radio: wextRadio}

// describe reads one interface's link, or, for a virtual interface over one
// piece of hardware, that hardware's.
func (s sysLinks) describe(name string) Link {
	name = s.physical(name, 0)
	dir := filepath.Join(s.net, name)
	link := Link{Name: name, Kind: Unknown}
	if _, err := os.Stat(dir); err != nil {
		// Gone between being found and being read, or never in sysfs.
		return link
	}
	link.SentBytes, link.Counted = readCount(filepath.Join(dir, "statistics", "tx_bytes"))

	switch {
	case present(dir, "wireless") || present(dir, "phy80211"):
		link.Kind = WiFi
		link.Mbps, link.FrequencyMHz = s.radio(name)
		link.SignalDBm = s.signal(name)
	case !present(dir, "device"):
		link.Kind = Virtual
	case readText(filepath.Join(dir, "type")) == "1": // ARPHRD_ETHER
		link.Kind = Ethernet
		link.Mbps = readRate(filepath.Join(dir, "speed"))
	}
	return link
}

// physical is the interface that puts name's traffic on a wire: name itself,
// or, for a virtual interface over exactly one piece of hardware — a VLAN, a
// bridge with one physical port, a bond of one — that hardware. Over several,
// or none, there is no one wire to speak of, and name stays as it is. The
// kernel names what an interface sits on with a lower_<name> link.
func (s sysLinks) physical(name string, depth int) string {
	if depth > 4 || present(filepath.Join(s.net, name), "device") {
		return name
	}
	entries, err := os.ReadDir(filepath.Join(s.net, name))
	if err != nil {
		return name
	}
	var under []string
	for _, e := range entries {
		lower, ok := strings.CutPrefix(e.Name(), "lower_")
		if !ok {
			continue
		}
		if below := s.physical(lower, depth+1); present(filepath.Join(s.net, below), "device") {
			under = append(under, below)
		}
	}
	if len(under) == 1 {
		return under[0]
	}
	return name
}

// signal is a Wi-Fi interface's signal strength, or 0.
func (s sysLinks) signal(name string) int {
	table, err := os.ReadFile(s.wireless)
	if err != nil {
		return 0
	}
	return wirelessSignal(string(table), name)
}

// wirelessSignal finds an interface's row in /proc/net/wireless and reads its
// signal level, which cfg80211 drivers give in dBm:
//
//	Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE
//	 face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22
//	 wlan0: 0000   54.  -56.  -256        0      0      0      0     35        0
//
// A level that is not negative is a driver's own scale, not dBm, and is not
// reported.
func wirelessSignal(table, name string) int {
	for _, line := range strings.Split(table, "\n") {
		iface, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(iface) != name {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 3 {
			return 0
		}
		level, err := strconv.Atoi(strings.TrimSuffix(fields[2], "."))
		if err != nil || level >= 0 || level < -120 {
			return 0
		}
		return level
	}
	return 0
}

// present says whether a sysfs entry exists. Lstat, because the entries that
// matter are links, and a link is what is being asked about.
func present(dir, entry string) bool {
	_, err := os.Lstat(filepath.Join(dir, entry))
	return err == nil
}

func readText(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// readRate reads an Ethernet link's rate in Mb/s. The kernel says -1, or
// refuses the read outright, for a link that does not know it.
func readRate(path string) int {
	n, err := strconv.Atoi(readText(path))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func readCount(path string) (uint64, bool) {
	n, err := strconv.ParseUint(readText(path), 10, 64)
	return n, err == nil
}
