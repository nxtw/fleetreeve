package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
)

func order(t *testing.T, cat *core.Catalog) string {
	t.Helper()
	if err := cat.Normalize(); err != nil {
		t.Fatal(err)
	}
	g, err := cat.Graph()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range g.Order {
		ids = append(ids, r.ID())
	}
	return strings.Join(ids, " ")
}

func TestEnrollCatalog(t *testing.T) {
	spec := enrollSpec{Server: "https://fleetreeve.example.com:8140/a%20b", Host: "web 1", Token: "tok", Binary: []byte{0, 1, 0xff}, Every: 15 * time.Minute, CA: []byte("PEM")}
	cat := enrollCatalog(spec)
	got := order(t, cat)
	// Everything the service needs is in place, and systemd has re-read the
	// unit, before the service is started.
	want := "file[/usr/local/bin/fleetreeve] file[/etc/fleetreeve] file[/etc/fleetreeve/token] file[/etc/fleetreeve/ca.pem] " +
		"file[/etc/systemd/system/fleetreeve-pull.service] exec[systemctl daemon-reload] service[fleetreeve-pull]"
	if got != want {
		t.Errorf("order:\n got %s\nwant %s", got, want)
	}
	var unit string
	for _, r := range cat.Resources {
		if r.Title == enrollUnit {
			unit = r.Str("content")
		}
		if r.Title == enrollToken && (r.Str("mode") != "0600" || !r.Bool("sensitive")) {
			t.Errorf("token file not protected: %v", r.Attrs)
		}
	}
	exec := `ExecStart=/usr/local/bin/fleetreeve pull -server https://fleetreeve.example.com:8140/a%%20b -host "web 1" -token-file /etc/fleetreeve/token -ca /etc/fleetreeve/ca.pem -every 15m0s -self-update -log-format json`
	if !strings.Contains(unit, exec+"\n") {
		t.Errorf("unit:\n%s", unit)
	}

	// Without a CA, a previously installed one is removed and not used.
	spec.CA = nil
	cat = enrollCatalog(spec)
	order(t, cat)
	for _, r := range cat.Resources {
		if r.Title == enrollCA && r.Str("ensure") != "absent" {
			t.Error("stale CA file is kept")
		}
		if r.Title == enrollUnit && strings.Contains(r.Str("content"), "-ca") {
			t.Error("unit references a CA that is not installed")
		}
	}

	// Removal stops the service before deleting what it runs from.
	if got := order(t, unenrollCatalog(false)); !strings.HasPrefix(got, "service[fleetreeve-pull] ") || !strings.HasSuffix(got, "file[/usr/local/bin/fleetreeve]") {
		t.Errorf("unenroll order: %s", got)
	}
}

// With the RPM installed, the package owns the binary and the unit;
// enrolment only writes configuration and starts the packaged service.
func TestEnrollPackaged(t *testing.T) {
	if !isPackaged(map[string]any{"fleetreeve": map[string]any{"packaged": true}}) || isPackaged(map[string]any{}) {
		t.Fatal("package detection")
	}
	cat := enrollCatalog(enrollSpec{Server: "https://fleetreeve.example.com:8140", Host: "web1", Token: "tok", Every: 15 * time.Minute, CA: []byte("PEM"), Packaged: true})
	got := order(t, cat)
	want := "file[/etc/systemd/system/fleetreeve-pull.service] file[/usr/local/bin/fleetreeve] exec[systemctl daemon-reload] " +
		"file[/etc/fleetreeve] file[/etc/fleetreeve/token] file[/etc/fleetreeve/pull.conf] file[/etc/fleetreeve/ca.pem] service[fleetreeve-pull]"
	if got != want {
		t.Errorf("order:\n got %s\nwant %s", got, want)
	}
	for _, r := range cat.Resources {
		if r.Title == enrollConf {
			want := "FLEETREEVE_SERVER=\"https://fleetreeve.example.com:8140\"\nFLEETREEVE_HOST=\"web1\"\nFLEETREEVE_TOKEN_FILE=/etc/fleetreeve/token\nFLEETREEVE_EVERY=15m0s\nFLEETREEVE_CA=/etc/fleetreeve/ca.pem\n"
			if !strings.HasSuffix(r.Str("content"), want) {
				t.Errorf("pull.conf:\n%s", r.Str("content"))
			}
		}
	}
	if got := order(t, unenrollCatalog(true)); strings.Contains(got, enrollBin) || strings.Contains(got, enrollUnit) || strings.Contains(got, enrollConf) {
		t.Errorf("removal touches package-owned files: %s", got)
	}
}

// enroll -rpm: the package is uploaded and installed before the service
// is configured and started, and an unpackaged enrolment is cleaned up.
func TestEnrollWithRPM(t *testing.T) {
	cat := enrollCatalog(enrollSpec{Server: "https://c:8140", Host: "web1", Token: "tok", Every: time.Minute, Packaged: true, RPM: []byte{0xed, 0xab, 0xee, 0xdb}})
	got := order(t, cat)
	want := "file[/var/cache/fleetreeve] file[/var/cache/fleetreeve/fleetreeve.rpm] exec[install fleetreeve package] " +
		"file[/etc/systemd/system/fleetreeve-pull.service] file[/usr/local/bin/fleetreeve] exec[systemctl daemon-reload] " +
		"file[/etc/fleetreeve] file[/etc/fleetreeve/token] file[/etc/fleetreeve/pull.conf] file[/etc/fleetreeve/ca.pem] service[fleetreeve-pull]"
	if got != want {
		t.Errorf("order:\n got %s\nwant %s", got, want)
	}
	for _, r := range cat.Resources {
		switch r.ID() {
		case "exec[install fleetreeve package]":
			if r.Str("unless") != "rpm -q fleetreeve" || !strings.Contains(r.Str("command"), "--replacepkgs") {
				t.Errorf("install step: %v", r.Attrs)
			}
		case "file[/usr/local/bin/fleetreeve]", "file[/etc/systemd/system/fleetreeve-pull.service]":
			if r.Str("ensure") != "absent" {
				t.Errorf("%s is kept next to the package", r.ID())
			}
		}
	}

	// Picking the package for a node's architecture.
	dir := t.TempDir()
	for _, f := range []string{"fleetreeve-0.1.0-1.x86_64.rpm", "fleetreeve-0.2.0-1.x86_64.rpm", "fleetreeve-0.2.0-1.aarch64.rpm", "fleetreeve-server-0.2.0-1.x86_64.rpm"} {
		os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "fleetreeve-0.1.0-1.x86_64.rpm"), old, old)
	if got, err := findRPM(dir, "amd64"); err != nil || filepath.Base(got) != "fleetreeve-0.2.0-1.x86_64.rpm" {
		t.Errorf("amd64: %q %v", got, err)
	}
	if got, err := findRPM(dir, "arm64"); err != nil || filepath.Base(got) != "fleetreeve-0.2.0-1.aarch64.rpm" {
		t.Errorf("arm64: %q %v", got, err)
	}
	if _, err := findRPM(filepath.Join(dir, "fleetreeve-0.2.0-1.aarch64.rpm"), "amd64"); err == nil {
		t.Error("package of the wrong architecture accepted")
	}
	if _, err := findRPM(t.TempDir(), "amd64"); err == nil || !strings.Contains(err.Error(), "make rpm") {
		t.Errorf("empty directory: %v", err)
	}
}

func TestEnrollable(t *testing.T) {
	root := map[string]any{"uid": float64(0)}
	if err := enrollable(map[string]any{"init": "systemd", "user": root}); err != nil {
		t.Error(err)
	}
	if err := enrollable(map[string]any{"init": "launchd", "user": root}); err == nil || !strings.Contains(err.Error(), "launchd") {
		t.Errorf("non-systemd host accepted: %v", err)
	}
	if err := enrollable(map[string]any{"init": "systemd", "user": map[string]any{"uid": float64(1000)}}); err == nil || !strings.Contains(err.Error(), "become") {
		t.Errorf("non-root accepted: %v", err)
	}
}

func TestUnenrollIsEnrollRemove(t *testing.T) {
	got := unalias([]string{"unenroll", "web1", "-plan"})
	if want := []string{"enroll", "-remove", "web1", "-plan"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unenroll web1 -plan became %v, want %v", got, want)
	}
	if got := unalias([]string{"enroll", "web1"}); !reflect.DeepEqual(got, []string{"enroll", "web1"}) {
		t.Errorf("enroll was rewritten to %v", got)
	}
	// It is the enroll command from there on: the same settings apply.
	o, err := parse("enroll", got[1:])
	if err != nil || !o.remove || !o.plan || len(o.args) != 1 || o.args[0] != "web1" {
		t.Errorf("parsed as %+v, %v", o, err)
	}
}
