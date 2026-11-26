package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nxtw/fleetreeve/internal/pull"
)

// `fleetreeve upgrade` brings this machine to a newer release: it fetches
// the release (or takes it from a directory), verifies what it fetched
// against the release's checksums, upgrades fleetreeve the way it was
// installed here (rpm, deb, or the plain binary) and, on a server, has
// the server and the console run the new version. The second half is
// carried out by the new binary: only it knows what its version needs.

// Where releases come from. Variables, so that tests can point elsewhere.
var (
	upgradeAPI       = "https://api.github.com/repos/nxtw/fleetreeve"
	upgradeDownloads = "https://github.com/nxtw/fleetreeve/releases/download"
	upgradeCache     = "/var/cache/fleetreeve/upgrade"
	upgradeHostRPMs  = "/usr/share/fleetreeve/rpm" // packages `enroll` gives to hosts, where a server keeps them
)

// release is one version of fleetreeve and a way to get its files.
type release struct {
	Version string            // without the v
	From    string            // for messages: where it comes from
	Sums    map[string]string // file name -> SHA-256; nil when a directory has no SHA256SUMS
	Names   []string          // the files it has
	get     func(ctx context.Context, name string) ([]byte, error)
}

// file returns a release file, verified against the release's checksums.
func (r *release) file(ctx context.Context, name string) ([]byte, error) {
	data, err := r.get(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	if r.Sums != nil {
		want, ok := r.Sums[name]
		if !ok {
			return nil, fmt.Errorf("%s is not in the release's SHA256SUMS", name)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
			return nil, fmt.Errorf("%s does not match its checksum in SHA256SUMS (got %s…, expected %s…): nothing was installed", name, got[:12], want[:12])
		}
	}
	return data, nil
}

// parseSums reads the output of sha256sum.
func parseSums(b []byte) map[string]string {
	sums := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 2 && len(f[0]) == 64 {
			sums[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return sums
}

var packageVersion = regexp.MustCompile(`^fleetreeve[-_](\d+(?:\.\d+)*)-\d+[._]`)

// versionLess compares dotted versions number by number.
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

var validVersion = regexp.MustCompile(`^\d+(\.\d+)*$`)

// httpGet fetches a URL. Nothing about this machine or its user is sent.
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fleetreeve/"+Version)
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}

// openRelease finds the release to upgrade to: in a directory (-from), or
// published on GitHub - the one named with -version, or the newest.
func openRelease(ctx context.Context, from, version string) (*release, error) {
	version = strings.TrimPrefix(version, "v")
	if version != "" && !validVersion.MatchString(version) {
		return nil, fmt.Errorf("-version: %q is not a version (expected something like 0.1.2)", version)
	}
	if from != "" {
		entries, err := os.ReadDir(from)
		if err != nil {
			return nil, err
		}
		r := &release{Version: version, From: from, get: func(_ context.Context, name string) ([]byte, error) {
			return os.ReadFile(filepath.Join(from, name))
		}}
		for _, e := range entries {
			if e.Type().IsRegular() {
				r.Names = append(r.Names, e.Name())
				// Without -version: the newest version the directory holds.
				if m := packageVersion.FindStringSubmatch(e.Name()); m != nil && version == "" && (r.Version == "" || versionLess(r.Version, m[1])) {
					r.Version = m[1]
				}
			}
		}
		if b, err := os.ReadFile(filepath.Join(from, "SHA256SUMS")); err == nil {
			r.Sums = parseSums(b)
		}
		if r.Version == "" {
			return nil, fmt.Errorf("%s holds no fleetreeve packages to tell the version from; name it with -version", from)
		}
		return r, nil
	}
	if version == "" {
		b, err := httpGet(ctx, upgradeAPI+"/releases/latest")
		if err != nil {
			return nil, fmt.Errorf("asking for the newest release: %v (-version names one; -from <dir> takes files you fetched yourself)", err)
		}
		var latest struct {
			Tag string `json:"tag_name"`
		}
		if err := json.Unmarshal(b, &latest); err != nil || !validVersion.MatchString(strings.TrimPrefix(latest.Tag, "v")) {
			return nil, fmt.Errorf("asking for the newest release: unexpected answer")
		}
		version = strings.TrimPrefix(latest.Tag, "v")
	}
	base := upgradeDownloads + "/v" + version + "/"
	b, err := httpGet(ctx, base+"SHA256SUMS")
	if err != nil {
		return nil, fmt.Errorf("release %s: fetching its checksums: %v", version, err)
	}
	site := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(upgradeDownloads, "https://"), "http://"), "/releases/download")
	r := &release{Version: version, From: site, Sums: parseSums(b),
		get: func(ctx context.Context, name string) ([]byte, error) { return httpGet(ctx, base+name) }}
	if len(r.Sums) == 0 {
		return nil, fmt.Errorf("release %s: its SHA256SUMS lists no files", version)
	}
	for name := range r.Sums {
		r.Names = append(r.Names, name)
	}
	sort.Strings(r.Names)
	return r, nil
}

// installation is how fleetreeve got onto this machine.
type installation struct {
	Kind     string   // rpm, deb or binary
	Packages []string // the fleetreeve packages installed, for rpm and deb
	Binary   string   // the running binary
}

func quietly(ctx context.Context, name string, args ...string) (string, bool) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err == nil
}

func howInstalled(ctx context.Context) installation {
	in := installation{Kind: "binary", Binary: selfPath()}
	if real, err := filepath.EvalSymlinks(in.Binary); err == nil {
		in.Binary = real
	}
	if _, ok := quietly(ctx, "rpm", "-qf", in.Binary); ok {
		in.Kind, in.Packages = "rpm", []string{"fleetreeve"}
		if _, ok := quietly(ctx, "rpm", "-q", "fleetreeve-server"); ok {
			in.Packages = append(in.Packages, "fleetreeve-server")
		}
	} else if _, ok := quietly(ctx, "dpkg-query", "-S", in.Binary); ok {
		in.Kind, in.Packages = "deb", []string{"fleetreeve"}
		if st, _ := quietly(ctx, "dpkg-query", "-W", "-f=${Status}", "fleetreeve-server"); strings.Contains(st, "ok installed") {
			in.Packages = append(in.Packages, "fleetreeve-server")
		}
	}
	return in
}

var rpmArch = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}

// filesFor picks, from a release, the files that upgrade this installation.
func filesFor(in installation, r *release, goos, goarch string) ([]string, error) {
	var patterns []string
	switch in.Kind {
	case "rpm":
		for _, p := range in.Packages {
			patterns = append(patterns, p+"-"+r.Version+"-*."+rpmArch[goarch]+".rpm")
		}
	case "deb":
		for _, p := range in.Packages {
			patterns = append(patterns, p+"_"+r.Version+"-*_"+goarch+".deb")
		}
	default:
		patterns = []string{"fleetreeve_" + goos + "_" + goarch + ".tar.gz"}
	}
	var files []string
	for _, pat := range patterns {
		found := ""
		for _, name := range r.Names {
			if ok, _ := filepath.Match(pat, name); ok {
				found = name
			}
		}
		if found == "" {
			return nil, fmt.Errorf("release %s (%s) has no %s, which this machine needs", r.Version, r.From, pat)
		}
		files = append(files, found)
	}
	return files, nil
}

// hostRPMs are the client packages of a release, for every architecture.
func hostRPMs(r *release) []string {
	var files []string
	for _, name := range r.Names {
		if ok, _ := filepath.Match("fleetreeve-"+r.Version+"-*.rpm", name); ok {
			files = append(files, name)
		}
	}
	return files
}

// unpackBinary replaces the binary, and the builds for hosts next to it
// when it has them there, with those from a release archive.
func unpackBinary(archive []byte, binary string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	agents := filepath.Join(filepath.Dir(binary), "agents")
	_, noAgents := os.Stat(agents)
	replace := func(path string, from io.Reader) error {
		tmp := path + ".upgrade"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, io.LimitReader(from, 512<<20)); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(tmp, path) // the running process keeps the old file
	}
	done := false
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(h.Name)), "./")
		switch {
		case h.Typeflag != tar.TypeReg:
		case name == "fleetreeve":
			if err := replace(binary, tr); err != nil {
				return err
			}
			done = true
		case noAgents == nil && filepath.Dir(name) == "agents" && strings.HasPrefix(filepath.Base(name), "fleetreeve-"):
			if err := replace(filepath.Join(agents, filepath.Base(name)), tr); err != nil {
				return err
			}
		}
	}
	if !done {
		return fmt.Errorf("the archive holds no fleetreeve binary")
	}
	return nil
}

// serverHere says how a fleetreeve server runs on this machine: "container"
// (made by setup), "foreign" (a container someone set up by hand),
// "service", or "" when there is none.
func serverHere() string {
	if def, err := os.ReadFile(setupContainer); err == nil {
		if strings.Contains(string(def), ctrWritten) {
			return "container"
		}
		return "foreign"
	}
	for _, unit := range []string{filepath.Join(setupUnitDir, "fleetreeve-server.service"), "/usr/lib/systemd/system/fleetreeve-server.service"} {
		if _, err := os.Stat(unit); err == nil {
			if out, _ := exec.Command("systemctl", "is-enabled", "fleetreeve-server.service").Output(); strings.HasPrefix(string(out), "enabled") {
				return "service"
			}
		}
	}
	return ""
}

func upgrade(ctx context.Context, o *options) error {
	if len(o.args) == 1 && o.args[0] == "finish" {
		return upgradeFinish(ctx, o)
	}
	if len(o.args) > 0 {
		return fmt.Errorf("usage: fleetreeve upgrade [-version <v>] [-from <dir>] [-with-hosts] [-plan] [-yes]")
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("upgrade is for Linux machines; on %s, install the new release the way you installed this one", runtime.GOOS)
	}
	in := howInstalled(ctx)
	rel, err := openRelease(ctx, o.from, o.modVersion)
	if err != nil {
		return err
	}
	fmt.Printf("fleetreeve %s is installed here (%s). ", Version, map[string]string{
		"rpm": "RPM package", "deb": "Debian package", "binary": "the binary " + in.Binary}[in.Kind])
	same := rel.Version == Version
	switch {
	case same:
		fmt.Printf("That is the release from %s as well.\n", rel.From)
	case versionLess(rel.Version, Version) && o.modVersion == "":
		fmt.Println()
		return fmt.Errorf("%s only has %s, which is older; name it with -version if going back is what you want", rel.From, rel.Version)
	default:
		fmt.Printf("%s has %s.\n", rel.From, rel.Version)
	}
	server := serverHere()
	_, keepsRPMs := os.Stat(upgradeHostRPMs)
	var files, forHosts []string
	if !same {
		if files, err = filesFor(in, rel, runtime.GOOS, runtime.GOARCH); err != nil {
			return err
		}
	}
	if server != "" && (keepsRPMs == nil || o.withHosts) {
		forHosts = hostRPMs(rel)
	}
	verified := "verify the release's checksums"
	if rel.Sums == nil {
		verified = "use what is there as it is (the directory has no SHA256SUMS)"
	}
	fmt.Println("\nAbout to:")
	if len(files) > 0 {
		fmt.Printf("  - take %s from %s and %s\n", strings.Join(files, ", "), rel.From, verified)
		fmt.Printf("  - %s\n", map[string]string{"rpm": "install with rpm", "deb": "install with dpkg", "binary": "replace " + in.Binary}[in.Kind])
	}
	switch server {
	case "container":
		fmt.Println("  - make the container image from the new version and restart the server and the console")
	case "service":
		fmt.Println("  - restart the server and the console")
	case "foreign":
		fmt.Println("  - leave the server's container alone: it was not made by `fleetreeve setup`, so its image is yours to update")
	}
	if len(forHosts) > 0 && keepsRPMs == nil {
		fmt.Printf("  - put the %s packages for hosts in %s\n", rel.Version, upgradeHostRPMs)
	}
	if o.withHosts {
		if server == "" {
			return fmt.Errorf("-with-hosts is for a fleetreeve server, and this machine does not run one")
		}
		fmt.Println("  - upgrade the hosts: `fleetreeve enroll` with the new package, for every host in the inventory")
	}
	if len(files) == 0 && server == "" {
		fmt.Println("  - nothing: this machine is up to date")
		return nil
	}
	if o.plan {
		fmt.Println("\nNothing was changed (-plan).")
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("upgrade replaces the installed fleetreeve and restarts services: run it with sudo")
	}
	a := &asker{in: bufio.NewReader(os.Stdin), yes: o.yes}
	if !a.confirm("Go ahead", true) {
		return fmt.Errorf("nothing was changed")
	}
	fmt.Println()

	// Everything is fetched and verified before anything is installed.
	dir := filepath.Join(upgradeCache, rel.Version)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var paths []string
	for _, name := range append(append([]string{}, files...), forHosts...) {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			continue // needed for this machine and for hosts alike
		}
		data, err := rel.file(ctx, name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	for _, name := range files {
		paths = append(paths, filepath.Join(dir, name))
	}
	if n := len(files) + len(forHosts); n > 0 {
		what := "verified against the release's checksums"
		if rel.Sums == nil {
			what = "not verified: no SHA256SUMS"
		}
		step("Release %s fetched (%s)", rel.Version, what)
	}

	run := func(args ...string) error {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	if len(files) > 0 {
		switch in.Kind {
		case "rpm":
			// One transaction: the server package wants the client of
			// the same version. --oldpackage: -version may name an older one.
			err = run(append([]string{"rpm", "-U", "--replacepkgs", "--oldpackage"}, paths...)...)
		case "deb":
			err = run(append([]string{"dpkg", "-i"}, paths...)...)
		default:
			var archive []byte
			if archive, err = os.ReadFile(paths[0]); err == nil {
				err = unpackBinary(archive, in.Binary)
			}
		}
		if err != nil {
			return fmt.Errorf("installing %s failed, and fleetreeve %s is still what is installed: %v", rel.Version, Version, err)
		}
		step("fleetreeve %s installed", rel.Version)
	}
	if len(forHosts) > 0 && keepsRPMs == nil {
		// Only this release's packages stay: enroll takes what is there.
		old, _ := filepath.Glob(filepath.Join(upgradeHostRPMs, "fleetreeve-[0-9]*.rpm"))
		for _, name := range forHosts {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(upgradeHostRPMs, name), b, 0o644); err != nil {
				return err
			}
		}
		for _, p := range old {
			if ok, _ := filepath.Match("fleetreeve-"+rel.Version+"-*.rpm", filepath.Base(p)); !ok {
				os.Remove(p)
			}
		}
		step("Packages for hosts in %s", upgradeHostRPMs)
	}

	// The rest is for the new version to do.
	args := []string{"upgrade", "finish"}
	if o.withHosts {
		args = append(args, "-with-hosts")
		if len(forHosts) > 0 {
			args = append(args, "-rpm", dir)
		}
	}
	next := exec.CommandContext(ctx, in.Binary, args...)
	next.Stdin, next.Stdout, next.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := next.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("fleetreeve %s is installed, but the steps after that did not all succeed (see above); `sudo fleetreeve upgrade` again repeats them", rel.Version)
		}
		return err
	}
	return nil
}

var imageLine = regexp.MustCompile(`(?m)^Image=localhost/fleetreeve:\S+$`)

// useImage points a container definition written by setup at an image.
func useImage(def, image string) string {
	return imageLine.ReplaceAllString(def, "Image="+image)
}

// serverAddress is where this machine's server answers, and the CA to
// verify it with, read from what setup wrote for the console or for the
// machine's own pull service.
func serverAddress() (url, ca string) {
	for _, file := range []string{setupEnv, enrollConf} {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
			v = strings.Trim(v, `"`)
			switch k {
			case "FLEETREEVE_SERVER":
				url = v
			case "FLEETREEVE_CA":
				ca = v
			}
		}
		if url != "" {
			return url, ca
		}
	}
	return "", ""
}

// serverRuns waits until the server's service is up and, where its
// address is known, until it answers as this version.
func serverRuns(ctx context.Context) error {
	var client *pull.Client
	if url, ca := serverAddress(); url != "" {
		client, _ = pull.NewClient(url, "", "", ca, false)
	}
	var last error
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
		state, _ := exec.CommandContext(ctx, "systemctl", "is-active", "fleetreeve-server.service").Output()
		if s := strings.TrimSpace(string(state)); s != "active" {
			last = fmt.Errorf("its service is %s", s)
			continue
		}
		if client == nil {
			if i >= 10 { // up for a while: a server that cannot start stops at once
				return nil
			}
			continue
		}
		h, err := client.Health(ctx)
		if err != nil {
			last = err
			continue
		}
		if v := fmt.Sprint(h["version"]); v != Version {
			last = fmt.Errorf("it answers as version %s", v)
			continue
		}
		return nil
	}
	return last
}

// configDir is the directory with the server's configuration, where
// commands such as enroll are run.
func configDir() string {
	for file, key := range map[string]string{
		setupContainer: "ConditionPathExists=",
		filepath.Join(setupUnitDir, "fleetreeve-server.service"): "WorkingDirectory=",
	} {
		b, _ := os.ReadFile(file)
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), key); ok {
				if key == "ConditionPathExists=" {
					v = filepath.Dir(v)
				}
				if _, err := os.Stat(filepath.Join(v, ".fleetreeve.yaml")); err == nil {
					return v
				}
			}
		}
	}
	return ""
}

// upgradeFinish is the second half of an upgrade, run by the new binary:
// the services of a server on this machine, then the hosts if asked.
func upgradeFinish(ctx context.Context, o *options) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("upgrade restarts services: run it with sudo")
	}
	run := func(args ...string) error {
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
		return nil
	}
	server := serverHere()
	switch server {
	case "container":
		image, err := buildImage(ctx, selfPath())
		if err != nil {
			return err
		}
		for _, file := range []string{setupContainer, consoleQuad} {
			def, err := os.ReadFile(file)
			if err != nil {
				continue // no console here
			}
			if now := useImage(string(def), image); now != string(def) {
				if err := os.WriteFile(file, []byte(now), 0o644); err != nil {
					return err
				}
			}
		}
		step("Container image %s made", image)
		if err := run("systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := run("systemctl", "restart", "fleetreeve-server.service"); err != nil {
			return fmt.Errorf("%v%s", err, serverSaid(ctx))
		}
		if _, err := os.Stat(consoleQuad); err == nil {
			if err := run("systemctl", "restart", "fleetreeve-dashboard.service"); err != nil {
				return err
			}
		}
	case "service":
		if err := run("systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := run("systemctl", "restart", "fleetreeve-server.service"); err != nil {
			return fmt.Errorf("%v%s", err, serverSaid(ctx))
		}
		if err := run("systemctl", "try-restart", "fleetreeve-dashboard.service"); err != nil {
			return err
		}
	case "foreign":
		fmt.Printf("  · The server's container (%s) was not made by `fleetreeve setup`: update its image yourself.\n", setupContainer)
	}
	if server == "container" || server == "service" {
		if err := serverRuns(ctx); err != nil {
			return fmt.Errorf("the server was restarted but is not running version %s: %v%s", Version, err, serverSaid(ctx))
		}
		what := "Server"
		if out, _ := exec.CommandContext(ctx, "systemctl", "is-active", "fleetreeve-dashboard.service").Output(); strings.TrimSpace(string(out)) == "active" {
			what = "Server and console"
		}
		step("%s running %s", what, Version)
	}
	// This machine's own pull service; a package restarts it itself, and
	// then this does no harm.
	run("systemctl", "try-restart", enrollSvc+".service")

	dir := configDir()
	if !o.withHosts {
		if server != "" {
			where := ""
			if dir != "" {
				where = " (in " + dir + ")"
			}
			fmt.Printf("\nThe hosts are not upgraded yet. Hosts that got fleetreeve as a package keep their version until you run\n  sudo fleetreeve upgrade -with-hosts\nor `fleetreeve enroll -rpm <dir with the packages>`%s; other hosts fetch the new build from the server by themselves at their next run.\n", where)
		}
		return nil
	}
	if dir == "" {
		return fmt.Errorf("cannot tell where this server's configuration is: run `fleetreeve enroll` there yourself to upgrade the hosts")
	}
	args := []string{"enroll"}
	if o.rpm != "" {
		args = append(args, "-rpm", o.rpm)
	}
	fmt.Printf("\nThe hosts (fleetreeve %s in %s):\n", strings.Join(args, " "), dir)
	enroll := exec.CommandContext(ctx, selfPath(), args...)
	enroll.Dir = dir
	enroll.Stdin, enroll.Stdout, enroll.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := enroll.Run(); err != nil {
		return fmt.Errorf("not every host was upgraded (see above); `fleetreeve enroll` in %s repeats it", dir)
	}
	return nil
}
