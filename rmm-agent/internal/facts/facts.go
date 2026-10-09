// Package facts collects the host inventory the agent reports to Nexus. It
// reads native Linux sources (/proc, statfs, net.Interfaces) where it can and
// shells out only to read-only tools (systemctl, dpkg-query/rpm, named -v)
// where it must. Every collector is best effort: one that is unavailable or
// fails yields an empty value (omitted from the report) and never fails the
// report, so a partial report is always possible.
package facts

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Facts is the capability/hardware manifest. JSON tags are camelCase to match
// the Nexus report endpoint (and, transitively, Daedalus IT's machine profile).
type Facts struct {
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Kernel        string `json:"kernel"`
	Arch          string `json:"arch"`
	CPUCores      int    `json:"cpuCores"`
	MemoryMB      int    `json:"memoryMb"`
	DiskGB        int    `json:"diskGb"`
	AgentVersion  string `json:"agentVersion"`
	UptimeSeconds int64  `json:"uptimeSeconds"`

	// Richer facts (PROVIDERS.md §2). Each is omitted when its collector is
	// unavailable on this host or found nothing.
	MachineID  string      `json:"machineId,omitempty"`
	PublicIP   string      `json:"publicIp,omitempty"`
	Interfaces []Interface `json:"interfaces,omitempty"`
	Listening  []Listener  `json:"listening,omitempty"`
	Services   []Service   `json:"services,omitempty"`
	Packages   []Package   `json:"packages,omitempty"`
	DNSServer  *DNSServer  `json:"dnsServer,omitempty"`
}

// Collect gathers the basic host facts — cheap, native reads only. Used for
// enrollment.
func Collect(agentVersion string) Facts {
	host, _ := os.Hostname()
	return Facts{
		Hostname:      host,
		OS:            osName(),
		Kernel:        kernel(),
		Arch:          arch(),
		CPUCores:      runtime.NumCPU(),
		MemoryMB:      memoryMB(),
		DiskGB:        diskGB("/"),
		AgentVersion:  agentVersion,
		UptimeSeconds: uptimeSeconds(),
		MachineID:     MachineID(),
	}
}

// CollectFull gathers the basic facts plus the richer inventory: interfaces,
// public address, listening sockets, services, packages and the DNS server.
// It never fails; a collector that cannot run leaves its field empty.
func CollectFull(agentVersion string) Facts {
	f := Collect(agentVersion)
	f.Interfaces = Interfaces()
	f.PublicIP = PublicIPv4(f.Interfaces)
	f.Listening = Listening()
	f.Services = Services()
	f.Packages = Packages()
	f.DNSServer = DetectDNSServer()
	return f
}

// MachineID is the systemd machine id (/etc/machine-id, falling back to the
// D-Bus copy), which Nexus uses to re-adopt a reinstalled agent.
func MachineID() string {
	for _, p := range machineIDPaths {
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" && id != "uninitialized" {
				return id
			}
		}
	}
	return ""
}

var machineIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

func osName() string {
	f, err := os.Open("/etc/os-release")
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"")
			}
		}
	}
	return runtime.GOOS
}

func kernel() string {
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// arch maps Go's architecture names to the uname -m style values operators
// expect to see (x86_64, aarch64, …).
func arch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	default:
		return runtime.GOARCH
	}
}

func memoryMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return int(kb / 1024)
			}
		}
	}
	return 0
}

func diskGB(path string) int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	total := st.Blocks * uint64(st.Bsize)
	return int(total / (1024 * 1024 * 1024))
}

func uptimeSeconds() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) >= 1 {
		if f, err := strconv.ParseFloat(fields[0], 64); err == nil {
			return int64(f)
		}
	}
	return 0
}
