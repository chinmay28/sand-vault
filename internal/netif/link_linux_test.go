//go:build linux

package netif

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// sysTree builds a copy of the parts of /sys/class/net this package reads.
type sysTree struct {
	t    *testing.T
	root string
}

func newSysTree(t *testing.T) *sysTree {
	return &sysTree{t: t, root: t.TempDir()}
}

func (s *sysTree) file(path, content string) {
	s.t.Helper()
	full := filepath.Join(s.root, "net", path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sysTree) dir(path string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Join(s.root, "net", path), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

// link makes an entry the kernel would make a symlink, pointing where the
// kernel's would: only its name is ever looked at.
func (s *sysTree) link(path string) {
	s.t.Helper()
	full := filepath.Join(s.root, "net", path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.Symlink("../../devices/somewhere", full); err != nil {
		s.t.Fatal(err)
	}
}

// ethernet adds a wired interface: hardware, of the Ethernet type.
func (s *sysTree) ethernet(name, speed string, sent string) {
	s.link(name + "/device")
	s.file(name+"/type", "1\n")
	if speed != "" {
		s.file(name+"/speed", speed+"\n")
	}
	if sent != "" {
		s.file(name+"/statistics/tx_bytes", sent+"\n")
	}
}

func (s *sysTree) links(radio func(string) (int, int)) sysLinks {
	if radio == nil {
		radio = func(string) (int, int) { return 0, 0 }
	}
	return sysLinks{net: filepath.Join(s.root, "net"), wireless: filepath.Join(s.root, "wireless"), radio: radio}
}

func TestAnEthernetLink(t *testing.T) {
	tree := newSysTree(t)
	tree.ethernet("enp5s0", "1000", "123456789")

	got := tree.links(nil).describe("enp5s0")
	want := Link{Name: "enp5s0", Kind: Ethernet, Mbps: 1000, SentBytes: 123456789, Counted: true}
	if got != want {
		t.Errorf("describe = %+v, want %+v", got, want)
	}
}

func TestAnEthernetLinkThatDoesNotKnowItsRate(t *testing.T) {
	tree := newSysTree(t)
	tree.ethernet("eth0", "-1", "5")
	tree.ethernet("eth1", "", "") // the kernel refuses to read speed with no carrier

	links := tree.links(nil)
	if got := links.describe("eth0"); got.Kind != Ethernet || got.Mbps != 0 || !got.Counted {
		t.Errorf("speed -1: %+v, want Ethernet with no rate, still counted", got)
	}
	if got := links.describe("eth1"); got.Kind != Ethernet || got.Mbps != 0 || got.Counted {
		t.Errorf("no speed, no count: %+v, want Ethernet with neither", got)
	}
}

func TestAWiFiLink(t *testing.T) {
	tree := newSysTree(t)
	tree.link("wlp4s0/device")
	tree.link("wlp4s0/phy80211")
	tree.dir("wlp4s0/wireless")
	tree.file("wlp4s0/type", "1\n")
	tree.file("wlp4s0/statistics/tx_bytes", "77\n")
	if err := os.WriteFile(filepath.Join(tree.root, "wireless"), []byte(
		"Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE\n"+
			" face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22\n"+
			"wlp4s0: 0000   54.  -52.  -256        0      0      0      0     35        0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var asked string
	radio := func(name string) (int, int) { asked = name; return 866, 5180 }

	got := tree.links(radio).describe("wlp4s0")
	want := Link{Name: "wlp4s0", Kind: WiFi, Mbps: 866, FrequencyMHz: 5180, SignalDBm: -52, SentBytes: 77, Counted: true}
	if got != want {
		t.Errorf("describe = %+v, want %+v", got, want)
	}
	if asked != "wlp4s0" {
		t.Errorf("the radio was asked about %q", asked)
	}
}

func TestAWiFiLinkKnownOnlyByItsPhy(t *testing.T) {
	// A kernel built without the wireless extensions has no "wireless"
	// directory, but cfg80211 still links the interface to its radio.
	tree := newSysTree(t)
	tree.link("wlan0/device")
	tree.link("wlan0/phy80211")
	tree.file("wlan0/type", "1\n")
	if got := tree.links(nil).describe("wlan0"); got.Kind != WiFi || got.SignalDBm != 0 {
		t.Errorf("describe = %+v, want Wi-Fi with no signal table to read", got)
	}
}

func TestAVirtualInterfaceOverOneWireIsThatWire(t *testing.T) {
	tree := newSysTree(t)
	tree.ethernet("enp5s0", "2500", "900")
	tree.file("veth1a2b/statistics/tx_bytes", "3\n") // a container's end: no device
	// A bridge with the NIC and a container's veth in it, and a VLAN on the
	// NIC: either way the traffic leaves on enp5s0.
	tree.link("br0/lower_enp5s0")
	tree.link("br0/lower_veth1a2b")
	tree.link("enp5s0.10/lower_enp5s0")
	// And a bridge over the VLAN, two virtual layers deep.
	tree.link("br1/lower_enp5s0.10")

	links := tree.links(nil)
	for _, name := range []string{"br0", "enp5s0.10", "br1"} {
		got := links.describe(name)
		if got.Name != "enp5s0" || got.Kind != Ethernet || got.Mbps != 2500 || got.SentBytes != 900 {
			t.Errorf("describe(%s) = %+v, want enp5s0's link", name, got)
		}
	}
}

func TestAVirtualInterfaceWithNoOneWireIsVirtual(t *testing.T) {
	tree := newSysTree(t)
	tree.ethernet("eth0", "1000", "1")
	tree.ethernet("eth1", "1000", "1")
	tree.file("veth9/statistics/tx_bytes", "1\n")
	tree.link("docker0/lower_veth9")  // only containers behind it
	tree.link("bond0/lower_eth0")     // two wires: neither is the link
	tree.link("bond0/lower_eth1")     //
	tree.file("wg0/speed", "10000\n") // a tunnel's made-up rate is not reported
	tree.file("veth9/speed", "10000\n")

	links := tree.links(nil)
	for _, name := range []string{"docker0", "bond0", "wg0", "veth9"} {
		got := links.describe(name)
		if got.Name != name || got.Kind != Virtual || got.Mbps != 0 {
			t.Errorf("describe(%s) = %+v, want itself, virtual, with no rate", name, got)
		}
	}
}

func TestHardwareThatIsNeitherEthernetNorWiFi(t *testing.T) {
	tree := newSysTree(t)
	tree.link("wwan0/device")
	tree.file("wwan0/type", "65534\n") // ARPHRD_NONE: a cellular modem
	tree.file("wwan0/speed", "100\n")
	if got := tree.links(nil).describe("wwan0"); got.Kind != Unknown || got.Mbps != 0 {
		t.Errorf("describe = %+v, want unknown, with no rate", got)
	}
}

func TestAnInterfaceThatIsNotThereAtAll(t *testing.T) {
	got := newSysTree(t).links(nil).describe("gone0")
	if got != (Link{Name: "gone0", Kind: Unknown}) {
		t.Errorf("describe = %+v", got)
	}
}

func TestReadingTheWiFiSignalTable(t *testing.T) {
	table := "Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE\n" +
		" face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22\n" +
		" wlan0: 0000   70.  -40.  -256        0      0      0      0      0        0\n" +
		"wlan01: 0000   30   -81   -256        0      0      0      0      0        0\n" +
		" wlan2: 0000   50.   60.     0        0      0      0      0      0        0\n" +
		" wlan3: 0000\n"
	for name, want := range map[string]int{"wlan0": -40, "wlan01": -81, "wlan2": 0, "wlan3": 0, "wlan9": 0, "face |": 0} {
		if got := wirelessSignal(table, name); got != want {
			t.Errorf("wirelessSignal(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestReadingTheDriversAnswers(t *testing.T) {
	answer := func(first uint32, second uint16) [16]byte {
		var data [16]byte
		binary.NativeEndian.PutUint32(data[0:4], first)
		binary.NativeEndian.PutUint16(data[4:6], second)
		return data
	}
	for bps, want := range map[uint32]int{866_700_000: 866, 2_402_000_000: 2402, 54_000_000: 54, 0: 0} {
		if got := wextMbps(answer(bps, 0)); got != want {
			t.Errorf("wextMbps(%d) = %d, want %d", bps, got, want)
		}
	}
	freqs := []struct {
		m    uint32
		e    uint16
		want int
	}{
		{5180, 6, 5180},           // cfg80211: megahertz, times a million
		{2412000, 3, 2412},        // an older driver's spelling of the same
		{243700000, 1, 2437},      // hertz in tens
		{6, 0, 0},                 // a channel number: no band in it
		{0, 6, 0},                 // nothing
		{5180, uint16(0xFFFF), 0}, // a negative exponent: nobody's frequency
	}
	for _, f := range freqs {
		if got := wextMHz(answer(f.m, f.e)); got != f.want {
			t.Errorf("wextMHz(m=%d, e=%d) = %d, want %d", f.m, int16(f.e), got, f.want)
		}
	}
}

func TestAskingADriverThatIsNotWiFi(t *testing.T) {
	// The loopback has no radio: the calls are made, and refused.
	if mbps, mhz := wextRadio("lo"); mbps != 0 || mhz != 0 {
		t.Errorf("wextRadio(lo) = %d, %d", mbps, mhz)
	}
	// A name the struct cannot hold is not asked about at all.
	if _, ok := wext(-1, "a-name-far-too-long-for-the-kernel", siocgiwrate); ok {
		t.Error("an overlong name was asked about")
	}
}
