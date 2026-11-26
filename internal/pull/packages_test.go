package pull

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestServerOffersClientPackages(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"fleetreeve-1.2.3-1.x86_64.rpm":        "rpm for x86_64",
		"fleetreeve-1.2.3-1.aarch64.rpm":       "rpm for aarch64",
		"fleetreeve_1.2.3-1_amd64.deb":         "deb for amd64",
		"fleetreeve-1.2.2-1.x86_64.rpm":        "an older version, left behind",
		"fleetreeve-server-1.2.3-1.x86_64.rpm": "the server package",
	} {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}
	s := &Server{Packages: dir, Version: "1.2.3", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	c, err := NewClient(srv.URL, "web1", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	info, err := c.PackageInfo(ctx, "rpm", "x86_64")
	if err != nil || info == nil {
		t.Fatalf("package info: %+v, %v", info, err)
	}
	if info.Version != "1.2.3" || info.File != "fleetreeve-1.2.3-1.x86_64.rpm" || info.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte("rpm for x86_64"))) {
		t.Errorf("package info: %+v", info)
	}
	if data, err := c.Package(ctx, "rpm", "x86_64"); err != nil || string(data) != "rpm for x86_64" {
		t.Errorf("the package: %q, %v", data, err)
	}
	if data, err := c.Package(ctx, "deb", "amd64"); err != nil || string(data) != "deb for amd64" {
		t.Errorf("the deb: %q, %v", data, err)
	}
	// None for this kind of host: not an error, there is just nothing.
	if info, err := c.PackageInfo(ctx, "deb", "arm64"); err != nil || info != nil {
		t.Errorf("a package the server lacks: %+v, %v", info, err)
	}
	// Nothing but client packages, by a fixed naming scheme.
	for _, name := range []string{"rpm-x86_64/../../secret", "rpm-..", "tar-amd64", "rpm-X86"} {
		resp, err := srv.Client().Get(srv.URL + "/v1/package/" + name)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("GET /v1/package/%s: http %d", name, resp.StatusCode)
		}
	}
	// A server of another version offers nothing from the same directory,
	// and one without a directory offers nothing at all.
	s.Version = "1.2.4"
	if info, err := c.PackageInfo(ctx, "rpm", "x86_64"); err != nil || info != nil {
		t.Errorf("packages of another version were offered: %+v, %v", info, err)
	}
	s.Version, s.Packages = "1.2.3", ""
	if info, err := c.PackageInfo(ctx, "rpm", "x86_64"); err != nil || info != nil {
		t.Errorf("a server without packages offered one: %+v, %v", info, err)
	}
}
