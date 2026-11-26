package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// releaseFiles is what a release of the given version carries.
func releaseFiles(version string) map[string][]byte {
	files := map[string][]byte{}
	for _, name := range []string{
		"fleetreeve-" + version + "-1.x86_64.rpm", "fleetreeve-" + version + "-1.aarch64.rpm",
		"fleetreeve-server-" + version + "-1.x86_64.rpm", "fleetreeve-server-" + version + "-1.aarch64.rpm",
		"fleetreeve_" + version + "-1_amd64.deb", "fleetreeve_" + version + "-1_arm64.deb",
		"fleetreeve-server_" + version + "-1_amd64.deb", "fleetreeve-server_" + version + "-1_arm64.deb",
		"fleetreeve_linux_amd64.tar.gz", "fleetreeve_linux_arm64.tar.gz", "fleetreeve_darwin_arm64.tar.gz",
	} {
		files[name] = []byte("content of " + name)
	}
	var sums strings.Builder
	for name, data := range files {
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	return files
}

// releaseSite serves releases the way GitHub does, and points upgrade at it.
func releaseSite(t *testing.T, latest string, releases map[string]map[string][]byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "fleetreeve/") {
			t.Errorf("request with User-Agent %q", ua)
		}
		if r.URL.Path == "/api/releases/latest" {
			fmt.Fprintf(w, `{"tag_name":"v%s","name":"ignored"}`, latest)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
		if len(parts) == 2 {
			if data, ok := releases[strings.TrimPrefix(parts[0], "v")][parts[1]]; ok {
				w.Write(data)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	api, dl := upgradeAPI, upgradeDownloads
	upgradeAPI, upgradeDownloads = srv.URL+"/api", srv.URL+"/download"
	t.Cleanup(func() { upgradeAPI, upgradeDownloads = api, dl })
}

func TestUpgradeFindsAndVerifiesARelease(t *testing.T) {
	ctx := context.Background()
	good, bad := releaseFiles("0.3.0"), releaseFiles("0.2.9")
	bad["fleetreeve-0.2.9-1.x86_64.rpm"] = []byte("not what the checksums were made of")
	releaseSite(t, "0.3.0", map[string]map[string][]byte{"0.3.0": good, "0.2.9": bad})

	// Without -version: the newest release.
	rel, err := openRelease(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "0.3.0" || len(rel.Names) != 11 {
		t.Fatalf("newest release: version %q with %d files", rel.Version, len(rel.Names))
	}
	if data, err := rel.file(ctx, "fleetreeve-0.3.0-1.x86_64.rpm"); err != nil || string(data) != "content of fleetreeve-0.3.0-1.x86_64.rpm" {
		t.Errorf("a file of the release: %q, %v", data, err)
	}
	// A named release, with or without the v. A file that is not what
	// the checksums say is refused; so is one the checksums do not list.
	rel, err = openRelease(ctx, "", "v0.2.9")
	if err != nil || rel.Version != "0.2.9" {
		t.Fatalf("-version v0.2.9: %+v, %v", rel, err)
	}
	if _, err := rel.file(ctx, "fleetreeve-0.2.9-1.x86_64.rpm"); err == nil || !strings.Contains(err.Error(), "does not match its checksum") {
		t.Errorf("a tampered file was accepted: %v", err)
	}
	if _, err := rel.file(ctx, "SHA256SUMS"); err == nil {
		t.Error("a file the checksums do not list was accepted")
	}
	if _, err := openRelease(ctx, "", "0.9.9"); err == nil {
		t.Error("a release that does not exist was found")
	}
	if _, err := openRelease(ctx, "", "../../x"); err == nil {
		t.Error("a version that is no version was accepted")
	}
}

func TestUpgradeFromADirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for name, data := range releaseFiles("0.3.0") {
		os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "fleetreeve-0.2.0-1.x86_64.rpm"), []byte("older, left behind"), 0o644)
	rel, err := openRelease(ctx, dir, "")
	if err != nil || rel.Version != "0.3.0" {
		t.Fatalf("version from the directory: %+v, %v", rel, err)
	}
	if _, err := rel.file(ctx, "fleetreeve_0.3.0-1_amd64.deb"); err != nil {
		t.Error(err)
	}
	os.WriteFile(filepath.Join(dir, "fleetreeve_0.3.0-1_amd64.deb"), []byte("changed since"), 0o644)
	if _, err := rel.file(ctx, "fleetreeve_0.3.0-1_amd64.deb"); err == nil {
		t.Error("a file that no longer matches SHA256SUMS was accepted")
	}
	// Without checksums the files are taken as they are, and that is said.
	os.Remove(filepath.Join(dir, "SHA256SUMS"))
	if rel, err = openRelease(ctx, dir, ""); err != nil || rel.Sums != nil {
		t.Fatalf("a directory without checksums: %+v, %v", rel, err)
	}
	if _, err := rel.file(ctx, "fleetreeve_0.3.0-1_amd64.deb"); err != nil {
		t.Error(err)
	}
	if _, err := openRelease(ctx, t.TempDir(), ""); err == nil {
		t.Error("an empty directory gave a release")
	}
}

func TestUpgradeFilesForThisMachine(t *testing.T) {
	rel := &release{Version: "0.3.0", From: "test"}
	for name := range releaseFiles("0.3.0") {
		rel.Names = append(rel.Names, name)
	}
	for _, c := range []struct {
		in       installation
		arch     string
		expected []string
	}{
		{installation{Kind: "rpm", Packages: []string{"fleetreeve"}}, "amd64", []string{"fleetreeve-0.3.0-1.x86_64.rpm"}},
		{installation{Kind: "rpm", Packages: []string{"fleetreeve", "fleetreeve-server"}}, "arm64",
			[]string{"fleetreeve-0.3.0-1.aarch64.rpm", "fleetreeve-server-0.3.0-1.aarch64.rpm"}},
		{installation{Kind: "deb", Packages: []string{"fleetreeve", "fleetreeve-server"}}, "amd64",
			[]string{"fleetreeve_0.3.0-1_amd64.deb", "fleetreeve-server_0.3.0-1_amd64.deb"}},
		{installation{Kind: "binary"}, "arm64", []string{"fleetreeve_linux_arm64.tar.gz"}},
	} {
		got, err := filesFor(c.in, rel, "linux", c.arch)
		if err != nil || !reflect.DeepEqual(got, c.expected) {
			t.Errorf("%s on %s: %v, %v; want %v", c.in.Kind, c.arch, got, err, c.expected)
		}
	}
	if _, err := filesFor(installation{Kind: "binary"}, rel, "linux", "riscv64"); err == nil {
		t.Error("a platform the release does not have got files")
	}
	// A server keeps every client package, rpm and deb, for hosts to
	// upgrade themselves with.
	if got, want := clientPackages(rel), []string{"fleetreeve-0.3.0-1.aarch64.rpm", "fleetreeve-0.3.0-1.x86_64.rpm", "fleetreeve_0.3.0-1_amd64.deb", "fleetreeve_0.3.0-1_arm64.deb"}; !reflect.DeepEqual(got, want) {
		t.Errorf("client packages: %v, want %v", got, want)
	}
	// Hosts get the client package, of every architecture, never the server's.
	got := hostRPMs(rel)
	if want := []string{"fleetreeve-0.3.0-1.aarch64.rpm", "fleetreeve-0.3.0-1.x86_64.rpm"}; !sameSet(got, want) {
		t.Errorf("packages for hosts: %v, want %v", got, want)
	}
}

func sameSet(a, b []string) bool {
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{{"0.1.1", "0.1.2", true}, {"0.1.9", "0.1.10", true}, {"0.2.0", "0.1.10", false}, {"1.0", "1.0.0", false}, {"0.1.1", "0.1.1", false}} {
		if got := versionLess(c.a, c.b); got != c.less {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestUpgradeUnpacksTheBinary(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range map[string]string{
		"./fleetreeve": "new binary", "./agents/fleetreeve-linux-amd64": "new amd64", "./agents/fleetreeve-linux-arm64": "new arm64",
		"./README.md": "readme", "./agents/../../outside": "escape",
	} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
		tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()

	// A binary on its own: only it is replaced.
	alone := t.TempDir()
	bin := filepath.Join(alone, "bin", "fleetreeve")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("old binary"), 0o755)
	if err := unpackBinary(buf.Bytes(), bin); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bin); string(b) != "new binary" {
		t.Errorf("the binary is %q", b)
	}
	if st, _ := os.Stat(bin); st.Mode().Perm() != 0o755 {
		t.Errorf("the binary has mode %v", st.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Dir(bin)); len(entries) != 1 {
		t.Errorf("besides the binary there now is: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(alone, "outside")); err == nil {
		t.Error("a file was written outside the binary's directory")
	}
	// With the builds for hosts next to it: those too.
	os.MkdirAll(filepath.Join(filepath.Dir(bin), "agents"), 0o755)
	if err := unpackBinary(buf.Bytes(), bin); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(bin), "agents", "fleetreeve-linux-arm64")); string(b) != "new arm64" {
		t.Errorf("the build for arm64 hosts is %q", b)
	}
	if err := unpackBinary([]byte("not an archive"), bin); err == nil {
		t.Error("something that is no archive was unpacked")
	}
}

func TestUpgradeUsesTheNewImage(t *testing.T) {
	def := "[Container]\n# a comment about Image=localhost/fleetreeve:0.1.1\nImage=localhost/fleetreeve:0.1.1\nContainerName=fleetreeve-server\n"
	got := useImage(def, "localhost/fleetreeve:0.3.0")
	if want := strings.Replace(def, "\nImage=localhost/fleetreeve:0.1.1", "\nImage=localhost/fleetreeve:0.3.0", 1); got != want {
		t.Errorf("got:\n%s", got)
	}
	// An image that is someone else's choice stays.
	other := "[Container]\nImage=ghcr.io/nxtw/fleetreeve-server:0.1.1\n"
	if useImage(other, "localhost/fleetreeve:0.3.0") != other {
		t.Error("an image setup did not make was replaced")
	}
}

// upgrade installs software as root. A settings file is found from the
// current directory upwards, so anyone who can write a .fleetreeve.yaml
// where an administrator runs the command could otherwise choose what
// gets installed.
func TestUpgradeIgnoresTheSettingsFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".fleetreeve.yaml"), []byte(
		"from: /tmp/planted\nyes: true\nversion: 9.9.9\nserver: https://example.test:8140\nupgrade:\n  from: /tmp/planted\n  with-hosts: true\n  yes: true\n  plan: true\n"), 0o644)
	t.Setenv("FLEETREEVE_CONFIG", "")
	t.Chdir(dir)
	o, err := parse("upgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.from != "" || o.yes || o.withHosts || o.modVersion != "" || o.plan || o.config != "" {
		t.Errorf("upgrade took settings from the file: from=%q yes=%v with-hosts=%v version=%q plan=%v (file %q)", o.from, o.yes, o.withHosts, o.modVersion, o.plan, o.config)
	}
	// The command line still decides.
	if o, err = parse("upgrade", []string{"-from", "/rel", "-yes"}); err != nil || o.from != "/rel" || !o.yes {
		t.Errorf("command line: %+v, %v", o, err)
	}
	// Other commands read the file as before.
	if o, err = parse("enroll", nil); err != nil || o.server != "https://example.test:8140" {
		t.Errorf("enroll no longer reads the settings file: %+v, %v", o, err)
	}
	// A file named in the environment is ignored as well.
	t.Setenv("FLEETREEVE_CONFIG", filepath.Join(dir, ".fleetreeve.yaml"))
	if o, err = parse("upgrade", nil); err != nil || o.from != "" || o.yes {
		t.Errorf("upgrade took settings from $FLEETREEVE_CONFIG: %+v, %v", o, err)
	}
}
