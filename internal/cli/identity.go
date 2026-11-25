package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/nxtw/fleetreeve/internal/remote"
)

// A fleetreeve server can have an identity of its own: a system user (by
// default "fleetreeve") that owns an SSH key pair generated on the server.
// Commands that connect to hosts then run as that user and log in to the
// hosts as a user of the same kind, so nothing depends on anyone's
// personal account or key:
//
//	sudo fleetreeve identity init          once, on the server
//	run-as: fleetreeve                     in .fleetreeve.yaml
//	fleetreeve enroll web3 -bootstrap-user root
//	                                  first contact with a host: logs in
//	                                  once as an existing account to create
//	                                  the user there and install the key
//
// After that, `fleetreeve plan`, `apply`, `enroll` and the like switch to the
// identity by themselves (through sudo) whoever types them.

const defaultIdentity = "fleetreeve"

var validUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)

// runAsCommands are the commands that connect to hosts or use the
// server's secret, and so run as the identity when one is configured.
var runAsCommands = map[string]bool{"plan": true, "apply": true, "enroll": true, "state": true, "facts": true,
	"run": true, "rollback": true, "lookup": true, "status": true, "token": true, "vault": true}

func currentUser() string {
	if u, err := osuser.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func selfPath() string {
	exe, err := os.Executable()
	if err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			return real
		}
	}
	return exe
}

// asIdentity builds the command line that runs fleetreeve as the identity.
func asIdentity(name string, args ...string) []string {
	argv := []string{"sudo", "-n", "-H", "-u", name}
	// Settings given in the environment go along; sudo drops them otherwise.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "FLEETREEVE_") || strings.HasPrefix(kv, "NO_COLOR=") {
			argv = append(argv, kv)
		}
	}
	return append(append(argv, selfPath()), args...)
}

// withoutFlag drops a flag and its value from a command line.
func withoutFlag(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := strings.TrimPrefix(args[i], "-")
		a = strings.TrimPrefix(a, "-")
		switch {
		case args[i] == "--":
			return append(out, args[i:]...)
		case strings.HasPrefix(args[i], "-") && a == name:
			i++ // its value
		case strings.HasPrefix(args[i], "-") && strings.HasPrefix(a, name+"="):
		default:
			out = append(out, args[i])
		}
	}
	return out
}

// switchUser hands the command over to the configured identity when it is
// not already running as it. For `enroll -bootstrap-user` the first
// contact with the hosts happens before that, as whoever typed the
// command, since only they can log in to a host that does not know the
// identity yet. It returns true when the command was dealt with.
func switchUser(ctx context.Context, cmd string, o *options, args []string) (int, bool) {
	fail := func(format string, a ...any) (int, bool) {
		fmt.Fprintf(os.Stderr, "fleetreeve: "+format+"\n", a...)
		return 1, true
	}
	if o.runAs == "" {
		if o.bootstrapUser != "" {
			return fail("-bootstrap-user creates the server's own user on a host, so it needs one: set run-as (see `fleetreeve help`), after `sudo fleetreeve identity init`")
		}
		return 0, false
	}
	if !validUser.MatchString(o.runAs) {
		return fail("run-as: %q is not a user name", o.runAs)
	}
	// Runs on this machine itself (no inventory) are root's, not the identity's.
	overSSH := cmd == "enroll" || cmd == "status" || cmd == "token" || o.inventory != ""
	if cmd == "vault" { // only the parts that use the server's secret
		overSSH = len(o.args) > 0 && (o.args[0] == "certs" || o.args[0] == "revoke")
	}
	if !runAsCommands[cmd] || !overSSH {
		return 0, false
	}
	me := currentUser()
	if cmd == "enroll" && o.bootstrapUser != "" && os.Getenv("FLEETREEVE_BOOTSTRAPPED") == "" {
		if err := bootstrap(ctx, o, me); err != nil {
			return fail("%v", err)
		}
		os.Setenv("FLEETREEVE_BOOTSTRAPPED", "1")
	}
	if me == o.runAs {
		return 0, false
	}
	if _, err := osuser.Lookup(o.runAs); err != nil {
		return fail("run-as: there is no user %s on this machine yet: run `sudo fleetreeve identity init` once", o.runAs)
	}
	if out, err := exec.Command("sudo", asIdentity(o.runAs, "version")[1:]...).CombinedOutput(); err != nil {
		return fail("cannot run as user %s: %s\n       (`sudo fleetreeve identity init` sets this up; you need to be in the group it names)", o.runAs, strings.TrimSpace(string(out)))
	}
	argv := asIdentity(o.runAs, append([]string{cmd}, withoutFlag(args, "bootstrap-user")...)...)
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return fail("run-as needs sudo: %v", err)
	}
	if err := syscall.Exec(sudo, argv, os.Environ()); err != nil {
		return fail("switching to user %s: %v", o.runAs, err)
	}
	return 0, true
}

// identityKey returns the identity's public key, asking the identity
// itself when this user may not read it.
func identityKey(name, me string) (string, error) {
	var out []byte
	var err error
	if me == name {
		var u *osuser.User
		if u, err = osuser.Lookup(name); err == nil {
			out, err = os.ReadFile(filepath.Join(u.HomeDir, ".ssh", "id_ed25519.pub"))
		}
	} else {
		argv := asIdentity(name, "identity", "key")
		out, err = exec.Command(argv[0], argv[1:]...).Output()
	}
	key := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(key, "ssh-") || strings.ContainsAny(key, "\n'") {
		return "", fmt.Errorf("the key of user %s could not be read (run `sudo fleetreeve identity init` once on this machine)", name)
	}
	return key, nil
}

// bootstrapScript makes a host ready for the identity: its user, the
// server's key as the only one that may log in as it, and sudo rights.
// It is run as root, changes nothing that is already right, and ends by
// printing the host's own keys so the server can recognise it afterwards.
//
// exclusive makes the server's key the only one that may log in as the
// user; without it the key is added to any that are there (for a machine
// that is itself a host of another server).
func bootstrapScript(name, key string, exclusive bool) string {
	only := "no"
	if exclusive {
		only = "yes"
	}
	return `set -eu
only=` + only + `
u='` + name + `'
key='` + key + `'
if ! id "$u" >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then
        useradd --system --create-home --shell /bin/sh --comment "fleetreeve configuration management" "$u"
    else
        adduser -S -D -s /bin/sh -g "fleetreeve configuration management" "$u"
    fi
fi
# No password, but not locked either: some systems refuse key logins to locked accounts.
usermod -p '*' "$u" 2>/dev/null || true
home=$(getent passwd "$u" | cut -d: -f6)
group=$(id -gn "$u")
[ -d "$home" ] || { mkdir -p "$home"; chown "$u:$group" "$home"; }
mkdir -p "$home/.ssh"
chown "$u:$group" "$home/.ssh"; chmod 0700 "$home/.ssh"
new="$home/.ssh/.authorized_keys.fleetreeve"
if [ "$only" = yes ] || [ ! -s "$home/.ssh/authorized_keys" ]; then
    printf '# Managed by fleetreeve: the key of the fleetreeve server. Changes are overwritten.\n%s\n' "$key" > "$new"
else
    { grep -vxF "$key" "$home/.ssh/authorized_keys" || true; printf '%s\n' "$key"; } > "$new"
fi
chown "$u:$group" "$new"; chmod 0600 "$new"
mv -f "$new" "$home/.ssh/authorized_keys"
if command -v restorecon >/dev/null 2>&1; then restorecon -R "$home/.ssh" 2>/dev/null || true; fi
mkdir -p /etc/sudoers.d
printf '# Managed by fleetreeve: the fleetreeve server makes changes on this host as this user.\n%s ALL=(ALL) NOPASSWD: ALL\n' "$u" > /etc/sudoers.d/.fleetreeve-new
chmod 0440 /etc/sudoers.d/.fleetreeve-new
if command -v visudo >/dev/null 2>&1; then visudo -cf /etc/sudoers.d/.fleetreeve-new >/dev/null; fi
mv -f /etc/sudoers.d/.fleetreeve-new /etc/sudoers.d/fleetreeve
# Does this host's sshd let the user in at all?
if command -v sshd >/dev/null 2>&1; then
    conf=$(sshd -T 2>/dev/null || true)
    groups=$(printf '%s\n' "$conf" | awk 'tolower($1)=="allowgroups"{for(i=2;i<=NF;i++)print $i}' | sort -u)
    users=$(printf '%s\n' "$conf" | awk 'tolower($1)=="allowusers"{for(i=2;i<=NF;i++)print $i}' | sort -u)
    if [ -n "$groups" ]; then
        ok=no
        for g in $(id -Gn "$u"); do if printf '%s\n' "$groups" | grep -qx "$g"; then ok=yes; fi; done
        [ "$ok" = yes ] || echo "FLEETREEVE-SSHD-LIMITS AllowGroups" $groups
    fi
    if [ -n "$users" ] && ! printf '%s\n' "$users" | grep -qx "$u"; then
        echo "FLEETREEVE-SSHD-LIMITS AllowUsers" $users
    fi
fi
echo "FLEETREEVE-HOST-KEYS"
cat /etc/ssh/ssh_host_*_key.pub 2>/dev/null || true
`
}

// bootstrap is the first contact with hosts: as the bootstrap user, with
// the caller's own means of logging in (their keys, agent or password).
func bootstrap(ctx context.Context, o *options, me string) error {
	if !validUser.MatchString(o.bootstrapUser) {
		return fmt.Errorf("-bootstrap-user: %q is not a user name", o.bootstrapUser)
	}
	if o.inventory == "" {
		return fmt.Errorf("enroll needs an inventory (-i) of hosts reachable over SSH")
	}
	key, err := identityKey(o.runAs, me)
	if err != nil {
		return err
	}
	sel := *o
	if len(o.args) > 0 {
		if o.limit != "" {
			return fmt.Errorf("name the hosts either as arguments or with -l, not both")
		}
		sel.limit = strings.Join(o.args, ",")
	}
	_, hosts, err := sel.hosts()
	if err != nil {
		return err
	}
	var trust bytes.Buffer
	for _, h := range hosts {
		if h.Connection != "ssh" {
			continue
		}
		login := *h
		login.User, login.Identity = o.bootstrapUser, ""
		become := false
		login.Become = &become
		run := "sh -s"
		if o.bootstrapUser != "root" {
			run = "sudo -n sh -s"
		}
		out, err := remote.Login(ctx, &login, run, bootstrapScript(o.runAs, key, true))
		if err != nil {
			hint := ""
			if strings.Contains(err.Error(), "sudo") {
				hint = fmt.Sprintf(" (%s needs sudo without a password there, or use -bootstrap-user root)", o.bootstrapUser)
			}
			return fmt.Errorf("%s: preparing the host as %s failed: %v%s", h.Name, o.bootstrapUser, err, hint)
		}
		fmt.Printf("%s: user %s is set up, with the key of this server\n", h.Name, o.runAs)
		for _, line := range strings.Split(out, "\n") {
			if limits, ok := strings.CutPrefix(line, "FLEETREEVE-SSHD-LIMITS "); ok {
				notef(slog.LevelWarn, "%s: its sshd does not let user %s in (%s): add the user or its group there, or the next step fails with \"Permission denied\"", h.Name, o.runAs, limits)
			}
		}
		// The host's keys, learnt over the caller's connection: the
		// identity knows the host by them from the start.
		if _, keys, ok := strings.Cut(out, "FLEETREEVE-HOST-KEYS\n"); ok {
			for _, line := range strings.Split(keys, "\n") {
				if f := strings.Fields(line); len(f) >= 2 && strings.HasPrefix(f[0], "ssh-") || len(f) >= 2 && strings.HasPrefix(f[0], "ecdsa-") {
					fmt.Fprintf(&trust, "%s %s %s\n", remote.KnownHostsName(h), f[0], f[1])
				}
			}
		}
	}
	if trust.Len() == 0 {
		return nil
	}
	if me == o.runAs {
		return trustHosts(&trust)
	}
	argv := asIdentity(o.runAs, "identity", "trust")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stderr = &trust, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("recording the hosts' keys for user %s: %v", o.runAs, err)
	}
	return nil
}

// trustHosts adds known_hosts lines from r to this user's, once each.
func trustHosts(r *bytes.Buffer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, "known_hosts")
	have, _ := os.ReadFile(p)
	known := map[string]bool{}
	for _, l := range strings.Split(string(have), "\n") {
		known[strings.TrimSpace(l)] = true
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if f3 := strings.Fields(line); len(f3) == 3 && !known[line] {
			known[line] = true
			if _, err := fmt.Fprintln(f, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// identity implements `fleetreeve identity init|show|key|trust`.
func identity(o *options) error {
	name := o.runAs
	if name == "" {
		name = defaultIdentity
	}
	if !validUser.MatchString(name) {
		return fmt.Errorf("%q is not a user name", name)
	}
	sub := ""
	if len(o.args) > 0 {
		sub = o.args[0]
	}
	switch sub {
	case "init":
		return identityInit(o, name)
	case "key":
		key, err := identityKey(name, currentUser())
		if err == nil {
			fmt.Println(key)
		}
		return err
	case "trust":
		var in bytes.Buffer
		if _, err := in.ReadFrom(os.Stdin); err != nil {
			return err
		}
		return trustHosts(&in)
	case "show", "":
		u, err := osuser.Lookup(name)
		if err != nil {
			return fmt.Errorf("there is no user %s on this machine: run `sudo fleetreeve identity init`", name)
		}
		key, err := identityKey(name, currentUser())
		if err != nil {
			return err
		}
		fmt.Printf("User:        %s (home %s)\nPrivate key: %s\nPublic key:  %s\n", name, u.HomeDir, filepath.Join(u.HomeDir, ".ssh", "id_ed25519"), key)
		return nil
	}
	return fmt.Errorf("usage: fleetreeve identity init|show|key   (init needs root; -run-as <user> names another user than %s)", defaultIdentity)
}

// identityInit creates the server's own user and its key pair, and lets
// administrators run fleetreeve as it. Running it again changes nothing that
// is already there; in particular it never replaces a key.
func identityInit(o *options, name string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("identity init creates a user: run it with sudo")
	}
	sh := func(args ...string) error {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	u, err := osuser.Lookup(name)
	if err != nil {
		home := "/var/lib/" + name + "-controller"
		if err := sh("useradd", "--system", "--create-home", "--home-dir", home, "--shell", "/bin/sh", "--comment", "fleetreeve configuration management", name); err != nil {
			return err
		}
		sh("usermod", "-p", "*", name) // no password; not locked, so key logins work everywhere
		if u, err = osuser.Lookup(name); err != nil {
			return err
		}
		fmt.Printf("Created user %s (home %s)\n", name, u.HomeDir)
	}
	// It must be able to log in to this machine too, when this machine is
	// one of its own hosts: a packaged service account comes without a shell.
	if out, err := exec.Command("getent", "passwd", name).Output(); err == nil {
		if f := strings.Split(strings.TrimSpace(string(out)), ":"); len(f) == 7 && (strings.HasSuffix(f[6], "nologin") || strings.HasSuffix(f[6], "false")) {
			sh("usermod", "-s", "/bin/sh", name)
		}
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	dir := filepath.Join(u.HomeDir, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	keyFile := filepath.Join(dir, "id_ed25519")
	if _, err := os.Stat(keyFile); err != nil {
		host, _ := os.Hostname()
		if err := sh("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name+"@"+host, "-f", keyFile); err != nil {
			return err
		}
		fmt.Printf("Generated a key pair: %s\n", keyFile)
	}
	for _, p := range []string{u.HomeDir, dir, keyFile, keyFile + ".pub"} {
		if err := os.Chown(p, uid, gid); err != nil {
			return err
		}
	}
	os.Chmod(dir, 0o700)
	os.Chmod(keyFile, 0o600)
	if _, err := exec.LookPath("restorecon"); err == nil {
		sh("restorecon", "-R", dir)
	}
	// Administrators may run fleetreeve as this user, and nothing else.
	admins := ""
	for _, g := range []string{"wheel", "sudo", "admin"} {
		if _, err := osuser.LookupGroup(g); err == nil {
			admins = g
			break
		}
	}
	if admins != "" {
		rule := fmt.Sprintf("# Managed by fleetreeve: members of %s may run fleetreeve as its own user, which owns the\n# key that logs in to the hosts.\n%%%s ALL=(%s) NOPASSWD:SETENV: %s\n", admins, admins, name, selfPath())
		tmp := "/etc/sudoers.d/.fleetreeve-controller-new"
		if err := os.WriteFile(tmp, []byte(rule), 0o440); err != nil {
			return err
		}
		if _, err := exec.LookPath("visudo"); err == nil {
			if err := sh("visudo", "-cf", tmp); err != nil {
				os.Remove(tmp)
				return err
			}
		}
		if err := os.Rename(tmp, "/etc/sudoers.d/fleetreeve-controller"); err != nil {
			return err
		}
		fmt.Printf("Members of group %s may run fleetreeve as %s (/etc/sudoers.d/fleetreeve-controller)\n", admins, name)
	} else {
		fmt.Printf("No wheel, sudo or admin group here: add a sudoers rule yourself so that administrators may run %s as %s\n", selfPath(), name)
	}
	// The identity needs the server's secret to enrol hosts.
	if o.secretFile != "" {
		if _, err := os.Stat(o.secretFile); err == nil {
			if err := os.Chown(o.secretFile, 0, gid); err == nil && os.Chmod(o.secretFile, 0o640) == nil {
				fmt.Printf("%s is now readable by root and %s only\n", o.secretFile, name)
			}
		}
	}
	pub, err := os.ReadFile(keyFile + ".pub")
	if err != nil {
		return err
	}
	fmt.Printf("\nPublic key:\n  %s\nNext: put `run-as: %s` in .fleetreeve.yaml and `user: %s` under `defaults:` in the inventory,\nthen `fleetreeve enroll <host> -bootstrap-user <an existing login>` for each host, once.\n", strings.TrimSpace(string(pub)), name, name)
	return nil
}
