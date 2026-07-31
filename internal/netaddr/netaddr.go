// Package netaddr answers "which URL do I tell someone to open?".
//
// Only the daemon can answer it. The TUI usually runs on another machine, a
// phone reads the URL off a screen, and which address works depends on where
// the viewer is: the loopback name on the tuner box itself, the address the
// router handed out for anyone else in the house, the tailnet address from
// outside it. So the daemon enumerates its own interfaces and reports every
// address, labelled by reach, instead of printing one guess.
package netaddr

import (
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
)

// Kind labels how far an address reaches. It is also the display order:
// the nearest way in comes first.
type Kind string

const (
	Local     Kind = "local"
	LAN       Kind = "lan"
	Tailscale Kind = "tailscale"
	Public    Kind = "public"
)

var kindOrder = map[Kind]int{Local: 0, LAN: 1, Tailscale: 2, Public: 3}

// Address is one way to reach this daemon.
type Address struct {
	Kind Kind   `json:"kind"`
	Host string `json:"host"` // "localhost" or a literal IPv4
	Base string `json:"base"` // scheme + host + port, no trailing slash
	// Iface is which interface carries it — worth showing when two
	// addresses look equally plausible and one of them is a VPN.
	Iface string `json:"iface,omitempty"`
}

// Iface is the part of a network interface this package looks at. Real ones
// come from net.Interfaces; tests build them by hand, because a test that
// asserts against whatever interfaces the build machine happens to have
// asserts nothing.
type Iface struct {
	Name     string
	Up       bool
	Loopback bool
	IPs      []net.IP
}

// Addresses lists every address this host can be reached at on port.
func Addresses(port int) []Address {
	return Classify(interfaces(), port)
}

// Classify turns interfaces into the labelled, ordered address list.
//
// localhost is always first and always present: it needs no interface to be
// up, and on the tuner box itself it is the address that cannot break.
func Classify(ifaces []Iface, port int) []Address {
	out := []Address{{Kind: Local, Host: "localhost", Base: base("localhost", port)}}
	seen := map[string]bool{"localhost": true}

	for _, ifc := range ifaces {
		if !ifc.Up || ifc.Loopback || isVirtual(ifc.Name) {
			continue
		}
		for _, ip := range ifc.IPs {
			// IPv4 only: an IPv6 literal has to be bracketed in a URL, and
			// every path that matters here (LAN, tailnet) has a v4 address.
			v4 := ip.To4()
			if v4 == nil || v4.IsLoopback() || v4.IsUnspecified() ||
				v4.IsLinkLocalUnicast() || v4.IsMulticast() {
				continue
			}
			host := v4.String()
			if seen[host] {
				continue
			}
			seen[host] = true
			out = append(out, Address{
				Kind:  kindOf(ifc.Name, v4),
				Host:  host,
				Base:  base(host, port),
				Iface: ifc.Name,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if kindOrder[out[i].Kind] != kindOrder[out[j].Kind] {
			return kindOrder[out[i].Kind] < kindOrder[out[j].Kind]
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// URL joins an address with a path on the daemon ("/stream.m3u8").
func (a Address) URL(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return a.Base + path
}

// cgnat is 100.64.0.0/10, which is where a tailnet address comes from — and
// the reliable signal, since the interface is not always named tailscale0
// (userspace mode, or a renamed one).
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func kindOf(iface string, ip net.IP) Kind {
	if strings.HasPrefix(iface, "tailscale") || cgnat.Contains(ip) {
		return Tailscale
	}
	if ip.IsPrivate() {
		return LAN
	}
	return Public
}

// virtualPrefixes name interfaces whose address no viewer can ever use:
// container and VM bridges answer only inside this host. docker0's
// 172.17.0.1 is an RFC1918 address like any other, so the name is the only
// thing that separates it from the real LAN. "br-" is docker's per-network
// bridge — the trailing dash matters, because a plain "br0" is a real LAN
// bridge, which is how a bridged host reaches the network at all.
var virtualPrefixes = []string{
	"docker", "br-", "veth", "virbr", "vnet", "vmnet", "vboxnet",
	"lxcbr", "lxdbr", "cni", "flannel", "kube", "tap",
}

func isVirtual(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func base(host string, port int) string {
	return fmt.Sprintf("http://%s:%d", host, port)
}

func interfaces() []Iface {
	list, err := net.Interfaces()
	if err != nil {
		// Not fatal: localhost still works, and it is what the box itself
		// needs. Losing the LAN address only costs a line on screen.
		slog.Warn("netaddr: cannot enumerate interfaces", "err", err)
		return nil
	}
	out := make([]Iface, 0, len(list))
	for _, ifc := range list {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		ips := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				ips = append(ips, ipn.IP)
			}
		}
		out = append(out, Iface{
			Name:     ifc.Name,
			Up:       ifc.Flags&net.FlagUp != 0,
			Loopback: ifc.Flags&net.FlagLoopback != 0,
			IPs:      ips,
		})
	}
	return out
}
