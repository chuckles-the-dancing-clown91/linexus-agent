package facts

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Listener is one listening TCP socket or unconnected (bound) UDP socket.
type Listener struct {
	Proto   string `json:"proto"` // "tcp" or "udp"
	Address string `json:"address"`
	Port    int    `json:"port"`
	Process string `json:"process"`
	inode   string
}

// maxListeners caps the list (a busy UDP host can bind thousands).
const maxListeners = 2000

// Socket states in /proc/net/{tcp,udp}*.
const (
	tcpListen = "0A"
	udpClose  = "07" // an unconnected UDP socket
)

// Listening reads /proc/net/{tcp,tcp6,udp,udp6} for listening sockets and
// names the owning process when /proc/<pid>/fd is readable.
func Listening() []Listener {
	var all []Listener
	for _, src := range []struct{ file, proto string }{
		{"tcp", "tcp"}, {"tcp6", "tcp"}, {"udp", "udp"}, {"udp6", "udp"},
	} {
		f, err := os.Open(filepath.Join(procRoot, "net", src.file))
		if err != nil {
			continue
		}
		all = append(all, ParseProcNet(f, src.proto, binary.NativeEndian)...)
		f.Close()
	}
	if len(all) == 0 {
		return nil
	}
	owners := socketOwners(procRoot)
	for i := range all {
		all[i].Process = owners[all[i].inode]
	}
	return dedupeListeners(all)
}

// ParseProcNet parses one /proc/net/{tcp,udp}[6] table, returning the TCP
// sockets in LISTEN and the UDP sockets that are bound but unconnected.
// order is the host byte order the kernel printed addresses in.
func ParseProcNet(r io.Reader, proto string, order binary.ByteOrder) []Listener {
	var out []Listener
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for sc.Scan() {
		if first { // header
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		local, remote, st, inode := f[1], f[2], f[3], f[9]
		switch proto {
		case "tcp":
			if st != tcpListen {
				continue
			}
		case "udp":
			if st != udpClose || !zeroEndpoint(remote) {
				continue
			}
		default:
			continue
		}
		ip, port, ok := parseHexEndpoint(local, order)
		if !ok {
			continue
		}
		out = append(out, Listener{Proto: proto, Address: ip, Port: port, inode: inode})
	}
	return out
}

func zeroEndpoint(s string) bool {
	return strings.Trim(s, "0:") == ""
}

// parseHexEndpoint decodes "0100007F:0016" (IPv4) or a 32-hex-digit IPv6
// address plus port. The kernel prints each 32-bit word of the address in
// host byte order; the port is a plain big-endian hex number.
func parseHexEndpoint(s string, order binary.ByteOrder) (string, int, bool) {
	addrHex, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return "", 0, false
	}
	raw, err := hex.DecodeString(addrHex)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return "", 0, false
	}
	ip := make(net.IP, len(raw))
	for w := 0; w < len(raw); w += 4 {
		v := order.Uint32(raw[w : w+4])
		binary.BigEndian.PutUint32(ip[w:w+4], v)
	}
	if v4 := ip.To4(); v4 != nil && len(raw) == 16 && !ip.Equal(net.IPv6zero) {
		// An IPv4-mapped IPv6 listener: keep it in IPv6 notation.
		return "::ffff:" + v4.String(), int(port), true
	}
	return ip.String(), int(port), true
}

// socketOwners maps socket inodes to process names by reading
// /proc/<pid>/fd symlinks ("socket:[12345]"). Processes whose fds are not
// readable (another user's, without root) are skipped.
func socketOwners(root string) map[string]string {
	owners := map[string]string{}
	pids, err := os.ReadDir(root)
	if err != nil {
		return owners
	}
	for _, p := range pids {
		if !p.IsDir() || !isDigits(p.Name()) {
			continue
		}
		fdDir := filepath.Join(root, p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		var comm string
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")
			if _, seen := owners[inode]; seen {
				continue
			}
			if comm == "" {
				b, _ := os.ReadFile(filepath.Join(root, p.Name(), "comm"))
				comm = strings.TrimSpace(string(b))
			}
			owners[inode] = comm
		}
	}
	return owners
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// dedupeListeners drops repeats (SO_REUSEPORT workers bind the same port many
// times), sorts by proto, port, address, and caps the list.
func dedupeListeners(in []Listener) []Listener {
	seen := map[string]int{}
	var out []Listener
	for _, l := range in {
		key := l.Proto + "|" + l.Address + "|" + strconv.Itoa(l.Port)
		if i, ok := seen[key]; ok {
			if out[i].Process == "" {
				out[i].Process = l.Process
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Address < b.Address
	})
	if len(out) > maxListeners {
		out = out[:maxListeners]
	}
	return out
}
