package facts

import (
	"net"
	"sort"
)

// Interface is one network interface and its addresses (CIDR strings).
type Interface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac"`
	Up        bool     `json:"up"`
	Loopback  bool     `json:"-"`
	Addresses []string `json:"addresses"`
}

// Interfaces lists the host's interfaces with their addresses.
func Interfaces() []Interface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]Interface, 0, len(ifs))
	for _, ifc := range ifs {
		it := Interface{
			Name:      ifc.Name,
			MAC:       ifc.HardwareAddr.String(),
			Up:        ifc.Flags&net.FlagUp != 0,
			Loopback:  ifc.Flags&net.FlagLoopback != 0,
			Addresses: []string{},
		}
		if addrs, err := ifc.Addrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok {
					it.Addresses = append(it.Addresses, ipn.String())
				}
			}
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// cgnat is the shared address space (RFC 6598), which is not public either.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// IsPublicIPv4 reports whether ip is a globally routable unicast IPv4
// address: not loopback, link-local, multicast, unspecified, RFC 1918 private
// or carrier-grade NAT space.
func IsPublicIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4.IsGlobalUnicast() && !v4.IsPrivate() && !cgnat.Contains(v4) && !v4.IsLoopback()
}

// PublicIPv4 picks the first public IPv4 address on an up, non-loopback
// interface (in interface-name order). It never makes outbound lookups: a
// host behind NAT honestly reports none.
func PublicIPv4(ifs []Interface) string {
	for _, it := range ifs {
		if it.Loopback || !it.Up {
			continue
		}
		for _, cidr := range it.Addresses {
			ip, _, err := net.ParseCIDR(cidr)
			if err != nil {
				ip = net.ParseIP(cidr)
			}
			if ip != nil && IsPublicIPv4(ip) {
				return ip.To4().String()
			}
		}
	}
	return ""
}
