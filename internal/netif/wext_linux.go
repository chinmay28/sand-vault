//go:build linux

package netif

import (
	"encoding/binary"
	"math"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A Wi-Fi link's rate and channel, asked with the wireless-extensions ioctls.
// They are the old interface, but every cfg80211 driver answers them while
// the kernel keeps the compatibility layer, which distribution kernels do,
// and they take a socket and a struct rather than a netlink conversation.
// Where they are not answered, the rate and channel are simply not reported.

const (
	siocgiwfreq = 0x8B05
	siocgiwrate = 0x8B21
)

// iwreq is the kernel's struct iwreq: an interface's name, and a union of
// answers sixteen bytes long, of which the two calls here fill the first
// eight.
type iwreq struct {
	name [unix.IFNAMSIZ]byte
	data [16]byte
}

// wextRadio asks a Wi-Fi interface's driver the rate it is sending at and the
// channel it is on, with zero for whatever it will not say.
func wextRadio(name string) (mbps, mhz int) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, 0
	}
	defer unix.Close(fd)

	if data, ok := wext(fd, name, siocgiwrate); ok {
		mbps = wextMbps(data)
	}
	if data, ok := wext(fd, name, siocgiwfreq); ok {
		mhz = wextMHz(data)
	}
	return mbps, mhz
}

// wext makes one wireless-extensions call about an interface.
func wext(fd int, name string, call uintptr) ([16]byte, bool) {
	var req iwreq
	if name == "" || len(name) >= len(req.name) {
		return req.data, false
	}
	copy(req.name[:], name)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), call, uintptr(unsafe.Pointer(&req)))
	return req.data, errno == 0
}

// wextMbps reads SIOCGIWRATE's answer, a struct iw_param whose value is the
// rate in bits a second. Read unsigned: cfg80211 works it out in 32 bits, and
// a link above 2.1 Gb/s comes back looking negative.
func wextMbps(data [16]byte) int {
	return int(binary.NativeEndian.Uint32(data[0:4]) / 1_000_000)
}

// wextMHz reads SIOCGIWFREQ's answer, a struct iw_freq: a mantissa and a
// power of ten, in hertz. With no exponent and a small mantissa it is a
// channel number instead, which does not say which band it is in.
func wextMHz(data [16]byte) int {
	m := int64(int32(binary.NativeEndian.Uint32(data[0:4])))
	e := int(int16(binary.NativeEndian.Uint16(data[4:6])))
	if m <= 0 || e < 0 || e > 9 || (e == 0 && m < 1000) {
		return 0
	}
	return int(math.Round(float64(m) * math.Pow10(e) / 1e6))
}
