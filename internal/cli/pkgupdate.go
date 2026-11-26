package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nxtw/fleetreeve/internal/pull"
)

// hostPackages is where a server keeps the client packages of its own
// version, for hosts to upgrade themselves with. `fleetreeve upgrade` fills
// it; in a container the image carries a copy.
const hostPackages = "/usr/share/fleetreeve/packages"

// packageCache is where a host puts a package it is about to install.
var packageCache = "/var/cache/fleetreeve"

// packageInstall is the command that installs a package file.
func packageInstall(kind, file string) []string {
	if kind == "deb" {
		return []string{"dpkg", "-i", file}
	}
	return []string{"rpm", "-U", file}
}

// packageUpdate keeps a host that has fleetreeve from a system package on
// its server's version: when the server offers a newer package, the host
// installs it and starts over as the new version. Like the self-update of
// other hosts, this grants the server nothing new - it already decides
// what runs here as root. It never goes back to an older version, and
// FLEETREEVE_AUTO_UPDATE=no in pull.conf turns it off for a host.
func packageUpdate(ctx context.Context, client *pull.Client) error {
	switch strings.ToLower(os.Getenv("FLEETREEVE_AUTO_UPDATE")) {
	case "no", "0", "false", "off":
		return nil
	}
	if !validVersion.MatchString(Version) {
		return nil // a development build: nothing to compare
	}
	in := howInstalled(ctx)
	if in.Kind == "binary" {
		return nil
	}
	arch := runtime.GOARCH
	if in.Kind == "rpm" {
		arch = rpmArch[runtime.GOARCH]
	}
	info, err := client.PackageInfo(ctx, in.Kind, arch)
	if err != nil || info == nil {
		return err
	}
	// FLEETREEVE_UPDATED_TO stops a loop should an update not take effect.
	if !validVersion.MatchString(info.Version) || !versionLess(Version, info.Version) || info.Version == os.Getenv("FLEETREEVE_UPDATED_TO") {
		return nil
	}
	data, err := client.Package(ctx, in.Kind, arch)
	if err != nil {
		return err
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != info.SHA256 {
		return fmt.Errorf("the downloaded package does not match its checksum")
	}
	if err := os.MkdirAll(packageCache, 0o755); err != nil {
		return err
	}
	file := filepath.Join(packageCache, "fleetreeve-update."+in.Kind)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return err
	}
	defer os.Remove(file)
	notef(slog.LevelInfo, "the server has fleetreeve %s and this host %s: installing the package", info.Version, Version)
	args := packageInstall(in.Kind, file)
	// Installing restarts this service, which ends everything the service
	// started - the installation with it, halfway. As a unit of its own
	// it is left alone.
	if _, err := exec.LookPath("systemd-run"); err == nil && os.Getenv("INVOCATION_ID") != "" {
		args = append([]string{"systemd-run", "--quiet", "--collect", "--wait", "--unit", "fleetreeve-update",
			"--description", "fleetreeve: upgrade to " + info.Version, "--"}, args...)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	// The package restarts this service when it is installed: everything
	// the service runs is told to stop, this process and the command it
	// waits for alike, in no particular order. That is the upgrade having
	// worked, not a failure, and the new version does the run.
	if err != nil {
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	if ctx.Err() != nil {
		notef(slog.LevelInfo, "fleetreeve %s is installed; the service restarts as the new version", info.Version)
		os.Remove(file)
		os.Exit(0)
	}
	if err != nil {
		return fmt.Errorf("installing fleetreeve %s failed: %s", info.Version, lastLines(string(out), 3))
	}
	// Still here: nothing restarted us (a run by hand). Carry on as the
	// new version.
	notef(slog.LevelInfo, "upgraded to fleetreeve %s, restarting", info.Version)
	os.Setenv("FLEETREEVE_UPDATED_TO", info.Version)
	return syscall.Exec(in.Binary, os.Args, os.Environ())
}

// lastLines is the end of a command's output, for a message.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}
