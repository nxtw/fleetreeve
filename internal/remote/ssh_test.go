package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nxtw/fleetreeve/internal/compile"
)

// Host keys are verified unless the host says otherwise.
func TestHostKeyCheck(t *testing.T) {
	off := false
	strict := strings.Join((&ssh{h: &compile.Host{Address: "10.0.0.1", User: "ops"}}).args(), " ")
	loose := strings.Join((&ssh{h: &compile.Host{Address: "10.0.0.1", User: "ops", HostKeyCheck: &off, SSHArgs: []string{"-o", "ProxyJump=gw"}}}).args(), " ")
	if strings.Contains(strict, "StrictHostKeyChecking") || !strings.HasSuffix(strict, " ops@10.0.0.1") {
		t.Errorf("default: %s", strict)
	}
	// Not verifying is done by learning the key first (trustPresentKey),
	// not by switching ssh's checks off.
	if strings.Contains(loose, "StrictHostKeyChecking") || !strings.Contains(loose, "ProxyJump=gw") {
		t.Errorf("host_key_check: false: %s", loose)
	}
	// What ssh-keyscan found is stored under the name ssh will look for.
	got := scanned("# 10.0.0.1:22 SSH-2.0-OpenSSH_9\n10.0.0.1 ssh-ed25519 AAAA1\n10.0.0.1 ecdsa-sha2-nistp256 AAAA2\n\n", "web.example.com")
	if strings.Join(got, "|") != "web.example.com ecdsa-sha2-nistp256 AAAA2|web.example.com ssh-ed25519 AAAA1" {
		t.Errorf("scanned: %v", got)
	}
	for want, h := range map[string]*compile.Host{
		"10.0.0.5":        {Address: "10.0.0.5", Port: 22},
		"[10.0.0.5]:2222": {Address: "10.0.0.5", Port: 2222},
		"web.example.com": {Address: "10.0.0.5", Port: 2222, SSHArgs: []string{"-o", "HostKeyAlias=web.example.com"}},
	} {
		if got := KnownHostsName(h); got != want {
			t.Errorf("KnownHostsName(%+v) = %q, want %q", h, got, want)
		}
	}
}

// Control sockets go where their full path - with what ssh adds - still
// fits a Unix socket; a directory that is too long is passed over.
func TestControlDir(t *testing.T) {
	base := t.TempDir()
	long := filepath.Join(base, strings.Repeat("x", 120))
	short := filepath.Join(os.TempDir(), "frcm-test")
	defer os.RemoveAll(short)
	if len(short) > controlDirMax {
		t.Skip("the temporary directory itself is too long here")
	}
	if got := firstControlDir([]string{long, short}); got != short {
		t.Errorf("chose %q", got)
	}
	if got := firstControlDir([]string{long}); got != "" {
		t.Errorf("a path too long for a socket was chosen: %q", got)
	}
	// The home of the server's own user, as setup makes it, leaves room.
	if dir := "/var/lib/fleetreeve-controller/.fleetreeve/cm"; len(dir) > controlDirMax {
		t.Errorf("%s is too long (%d > %d)", dir, len(dir), controlDirMax)
	}
}
