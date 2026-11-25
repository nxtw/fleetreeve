package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityHelpers(t *testing.T) {
	// The bootstrap flag is not passed on to the identity: the first
	// contact is the caller's.
	got := withoutFlag([]string{"web1", "-bootstrap-user", "root", "-plan", "--bootstrap-user=alice", "web2", "--", "-bootstrap-user", "x"}, "bootstrap-user")
	if want := "web1 -plan web2 -- -bootstrap-user x"; strings.Join(got, " ") != want {
		t.Errorf("withoutFlag: %q, want %q", strings.Join(got, " "), want)
	}
	for name, ok := range map[string]bool{"fleetreeve": true, "svc_fleetreeve-1": true, "": false, "Root": false, "a b": false, "x;rm": false, "-u": false} {
		if validUser.MatchString(name) != ok {
			t.Errorf("validUser(%q) != %v", name, ok)
		}
	}
	// What is run on a host is a valid script that sets up exactly this
	// user with exactly this key, and reports the host's keys.
	script := bootstrapScript("fleetreeve", "ssh-ed25519 AAAAC3Nza fleetreeve@server", true)
	for _, want := range []string{"u='fleetreeve'", "key='ssh-ed25519 AAAAC3Nza fleetreeve@server'", "authorized_keys", "/etc/sudoers.d/fleetreeve", "visudo -cf", "FLEETREEVE-HOST-KEYS"} {
		if !strings.Contains(script, want) {
			t.Errorf("the bootstrap script lacks %q", want)
		}
	}
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("the bootstrap script does not parse: %s", out)
	}
}

// Host keys learnt at first contact are remembered once each.
func TestTrustHosts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	lines := "10.0.0.5 ssh-ed25519 AAAA1\nweb.example.com ecdsa-sha2-nistp256 AAAA2\nnot a key line at all\n"
	for i := 0; i < 2; i++ {
		if err := trustHosts(bytes.NewBufferString(lines)); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(home, ".ssh", "known_hosts")
	got, _ := os.ReadFile(p)
	if string(got) != "10.0.0.5 ssh-ed25519 AAAA1\nweb.example.com ecdsa-sha2-nistp256 AAAA2\n" {
		t.Errorf("known_hosts:\n%s", got)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("known_hosts mode: %v", st.Mode())
	}
}
