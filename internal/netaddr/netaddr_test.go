package netaddr

import (
	"net"
	"testing"
)

func ips(list ...string) []net.IP {
	out := make([]net.IP, 0, len(list))
	for _, s := range list {
		ip := net.ParseIP(s)
		if ip == nil {
			panic("bad test ip " + s)
		}
		out = append(out, ip)
	}
	return out
}

// A single-box interface set of the shape this has to survive: a real LAN
// bridge, a tailnet, a stopped libvirt bridge, a docker bridge that is up,
// and a veth with nothing but a link-local address. Addresses are
// documentation values — a test pinned to one machine's real addressing
// would only assert where it was written.
func realWorldIfaces() []Iface {
	return []Iface{
		{Name: "lo", Up: true, Loopback: true, IPs: ips("127.0.0.1", "::1")},
		{Name: "enp2s0", Up: true},
		{Name: "wlp4s0", IPs: ips("192.168.1.9")}, // down
		{Name: "br0", Up: true, IPs: ips("192.168.1.42", "fe80::1")},
		{Name: "virbr0", IPs: ips("192.168.122.1")}, // down
		{Name: "tailscale0", Up: true, IPs: ips("100.101.102.103", "fd7a:115c:a1e0::1")},
		{Name: "docker0", Up: true, IPs: ips("172.17.0.1")},
		{Name: "veth0", Up: true, IPs: ips("fe80::2")},
	}
}

func TestClassifyPicksTheThreeAddressesThatMatter(t *testing.T) {
	got := Classify(realWorldIfaces(), 8010)

	want := []Address{
		{Kind: Local, Host: "localhost", Base: "http://localhost:8010"},
		{Kind: LAN, Host: "192.168.1.42", Base: "http://192.168.1.42:8010", Iface: "br0"},
		{Kind: Tailscale, Host: "100.101.102.103", Base: "http://100.101.102.103:8010", Iface: "tailscale0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d addresses, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("address %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// docker0 carries a perfectly ordinary RFC1918 address that is reachable
// from nowhere. Only its name gives it away, so this is the one filter that
// cannot be inferred from the address itself.
func TestContainerAndVMBridgesAreNotOffered(t *testing.T) {
	for _, name := range []string{"docker0", "br-1a2b3c", "virbr0", "vnet0", "vmnet8", "tap0"} {
		got := Classify([]Iface{{Name: name, Up: true, IPs: ips("172.17.0.1")}}, 8010)
		if len(got) != 1 || got[0].Kind != Local {
			t.Fatalf("%s: got %+v, want localhost only", name, got)
		}
	}
	// A plain br0 is a real LAN bridge, not a docker one.
	got := Classify([]Iface{{Name: "br0", Up: true, IPs: ips("192.168.1.42")}}, 8010)
	if len(got) != 2 || got[1].Kind != LAN {
		t.Fatalf("br0: got %+v, want a lan address", got)
	}
}

// A tailnet in userspace mode has no tailscale0 to recognise, so the address
// range has to be enough on its own.
func TestTailnetIsRecognisedByAddressRange(t *testing.T) {
	got := Classify([]Iface{{Name: "tun0", Up: true, IPs: ips("100.101.102.103")}}, 8010)
	if len(got) != 2 || got[1].Kind != Tailscale {
		t.Fatalf("got %+v, want the CGNAT address labelled tailscale", got)
	}
}

func TestDownInterfacesAndLinkLocalAreSkipped(t *testing.T) {
	got := Classify([]Iface{
		{Name: "eth0", IPs: ips("192.168.1.5")},                  // down
		{Name: "eth1", Up: true, IPs: ips("169.254.3.4", "::1")}, // link-local + v6 loopback
	}, 8010)
	if len(got) != 1 || got[0].Base != "http://localhost:8010" {
		t.Fatalf("got %+v, want localhost only", got)
	}
}

// localhost is unconditional: with no interfaces at all the box itself can
// still be told where to look.
func TestLocalhostIsAlwaysOffered(t *testing.T) {
	got := Classify(nil, 8010)
	if len(got) != 1 || got[0].Kind != Local || got[0].Host != "localhost" {
		t.Fatalf("got %+v", got)
	}
	if u := got[0].URL("/stream.m3u8"); u != "http://localhost:8010/stream.m3u8" {
		t.Fatalf("URL = %q", u)
	}
	if u := got[0].URL("stream.m3u8"); u != "http://localhost:8010/stream.m3u8" {
		t.Fatalf("URL without a leading slash = %q", u)
	}
}

// A duplicate address (the same IP on two interfaces, or an alias) must not
// produce two identical lines on screen.
func TestDuplicateAddressesCollapse(t *testing.T) {
	got := Classify([]Iface{
		{Name: "br0", Up: true, IPs: ips("192.168.1.42")},
		{Name: "enp2s0", Up: true, IPs: ips("192.168.1.42")},
	}, 8010)
	if len(got) != 2 {
		t.Fatalf("got %+v, want localhost + one lan address", got)
	}
}

// Real interfaces, so this only asserts what holds on any machine.
func TestAddressesOnThisHost(t *testing.T) {
	got := Addresses(8010)
	if len(got) == 0 || got[0].Kind != Local {
		t.Fatalf("got %+v, want localhost first", got)
	}
	for _, a := range got {
		if _, known := kindOrder[a.Kind]; !known || a.Base == "" || a.Host == "" {
			t.Fatalf("incomplete address %+v", a)
		}
	}
	// Kinds only: what a real host answers on is not this test's business,
	// and a -v run should not print it.
	kinds := make([]Kind, 0, len(got))
	for _, a := range got {
		kinds = append(kinds, a.Kind)
	}
	t.Logf("this host answers on %d address(es): %v", len(got), kinds)
}
