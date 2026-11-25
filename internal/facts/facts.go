// Package facts discovers properties of the node fleetreeve is running on.
package facts

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// FactsDir holds custom facts: every *.json file is merged under "custom".
const FactsDir = "/etc/fleetreeve/facts.d"

// Collect gathers the facts of the local machine.
func Collect() map[string]any {
	host, _ := os.Hostname()
	f := map[string]any{
		"hostname":   strings.SplitN(host, ".", 2)[0],
		"fqdn":       fqdn(host),
		"arch":       runtime.GOARCH,
		"os":         osFacts(),
		"kernel":     map[string]any{"name": runtime.GOOS, "release": out("uname", "-r")},
		"cpu":        map[string]any{"count": runtime.NumCPU()},
		"memory":     map[string]any{"total_mb": memoryMB()},
		"network":    network(),
		"init":       initSystem(),
		"selinux":    selinux(),
		"custom":     custom(),
		"fleetreeve": map[string]any{"packaged": packaged()},
	}
	u := map[string]any{"name": "", "uid": os.Getuid(), "home": ""}
	if cu, err := user.Current(); err == nil {
		u["name"], u["home"] = cu.Username, cu.HomeDir
	}
	f["user"] = u
	f["package_manager"] = ""
	for _, pm := range []string{"apt-get", "dnf", "yum", "apk", "pacman", "zypper", "brew"} {
		if _, err := exec.LookPath(pm); err == nil {
			f["package_manager"] = strings.TrimSuffix(pm, "-get")
			break
		}
	}
	// Round-trip through JSON so local and remote facts have identical types.
	b, _ := json.Marshal(f)
	var norm map[string]any
	_ = json.Unmarshal(b, &norm)
	return norm
}

func out(name string, args ...string) string {
	b, _ := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(b))
}

func fqdn(host string) string {
	if strings.Contains(host, ".") {
		return host
	}
	if f := out("hostname", "-f"); f != "" {
		return f
	}
	return host
}

func osFacts() map[string]any {
	o := map[string]any{"name": runtime.GOOS, "family": runtime.GOOS, "version": "", "codename": "", "pretty": runtime.GOOS}
	if runtime.GOOS == "darwin" {
		v := out("sw_vers", "-productVersion")
		o["name"], o["version"], o["pretty"] = "macos", v, "macOS "+v
		return o
	}
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return o
	}
	rel := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			rel[k] = strings.Trim(v, `"'`)
		}
	}
	o["name"], o["version"], o["codename"], o["pretty"] = rel["ID"], rel["VERSION_ID"], rel["VERSION_CODENAME"], rel["PRETTY_NAME"]
	o["family"] = rel["ID"]
	for _, id := range append([]string{rel["ID"]}, strings.Fields(rel["ID_LIKE"])...) {
		switch id {
		case "debian", "ubuntu":
			o["family"] = "debian"
		case "rhel", "fedora", "centos":
			o["family"] = "redhat"
		case "suse", "opensuse":
			o["family"] = "suse"
		case "arch":
			o["family"] = "arch"
		case "alpine":
			o["family"] = "alpine"
		default:
			continue
		}
		break
	}
	return o
}

func memoryMB() int {
	if runtime.GOOS == "darwin" {
		n, _ := strconv.ParseInt(out("sysctl", "-n", "hw.memsize"), 10, 64)
		return int(n / 1024 / 1024)
	}
	b, _ := os.ReadFile("/proc/meminfo")
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(f[1])
			return kb / 1024
		}
	}
	return 0
}

func network() map[string]any {
	ifaces := map[string]any{}
	first := ""
	list, _ := net.Interfaces()
	for _, i := range list {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		var ips []string
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				ips = append(ips, ipn.IP.String())
				if first == "" && ipn.IP.To4() != nil {
					first = ipn.IP.String()
				}
			}
		}
		if len(ips) > 0 {
			ifaces[i.Name] = ips
		}
	}
	// The source address of the default route; connecting a UDP socket
	// sends no packets.
	if c, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		first = c.LocalAddr().(*net.UDPAddr).IP.String()
		c.Close()
	}
	return map[string]any{"interfaces": ifaces, "ip": first}
}

func initSystem() string {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return "systemd"
	}
	if runtime.GOOS == "darwin" {
		return "launchd"
	}
	for _, c := range [][2]string{{"rc-service", "openrc"}, {"service", "sysv"}} {
		if _, err := exec.LookPath(c[0]); err == nil {
			return c[1]
		}
	}
	return "none"
}

// PackagedUnit is the pull service shipped by the fleetreeve package.
const PackagedUnit = "/usr/lib/systemd/system/fleetreeve-pull.service"

// packaged reports whether fleetreeve was installed on this node from a system
// package, in which case the package owns the binary and the service unit.
func packaged() bool {
	_, err := os.Stat(PackagedUnit)
	return err == nil
}

// selinux reports "enforcing", "permissive" or "disabled".
func selinux() string {
	b, err := os.ReadFile("/sys/fs/selinux/enforce")
	switch {
	case err != nil:
		return "disabled"
	case strings.TrimSpace(string(b)) == "1":
		return "enforcing"
	}
	return "permissive"
}

func custom() map[string]any {
	c := map[string]any{}
	files, _ := filepath.Glob(filepath.Join(FactsDir, "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			for k, v := range m {
				c[k] = v
			}
		}
	}
	return c
}
