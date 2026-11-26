package pull

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Hosts that have fleetreeve from a system package cannot replace their
// binary the way other hosts do: the package owns it. They are offered the
// package itself instead. The server only ever offers packages of its own
// version, out of the directory it was given, so "the server's version" is
// one thing for every kind of host.

// PackageInfo describes the client package a server offers for one kind of
// host.
type PackageInfo struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	File    string `json:"file"`
}

var packageRE = regexp.MustCompile(`^(rpm|deb)-([a-z0-9_]+)(\.json)?$`)

// findPackage returns the client package of this server's version for a
// packaging system ("rpm", "deb") and its name for an architecture
// ("x86_64", "amd64"), or "".
func (s *Server) findPackage(kind, arch string) string {
	if s.Packages == "" || s.Version == "" {
		return ""
	}
	pattern := "fleetreeve-" + s.Version + "-*." + arch + ".rpm"
	if kind == "deb" {
		pattern = "fleetreeve_" + s.Version + "-*_" + arch + ".deb"
	}
	entries, _ := os.ReadDir(s.Packages)
	for _, e := range entries {
		if ok, _ := filepath.Match(pattern, e.Name()); ok && e.Type().IsRegular() {
			return filepath.Join(s.Packages, e.Name())
		}
	}
	return ""
}

// pkg serves GET /v1/package/{kind}-{arch}: the client package, or with
// .json what it is. Like the builds, packages are public: no token.
func (s *Server) pkg(w http.ResponseWriter, r *http.Request) {
	m := packageRE.FindStringSubmatch(r.PathValue("file"))
	if m == nil {
		http.NotFound(w, r)
		return
	}
	path := s.findPackage(m[1], m[2])
	if path == "" {
		apiError(w, fmt.Sprintf("the server has no fleetreeve %s package for %s", m[1], m[2]), http.StatusNotFound)
		return
	}
	if m[3] == "" {
		s.Log.Info("package download", "remote", r.RemoteAddr, "package", filepath.Base(path))
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, path)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		apiError(w, "reading the package failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, PackageInfo{Version: s.Version, SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), File: filepath.Base(path)})
}

// PackageInfo asks what client package the server offers. It returns nil
// when the server offers none for this kind of host, or is too old to.
func (c *Client) PackageInfo(ctx context.Context, kind, arch string) (*PackageInfo, error) {
	b, err := c.do(ctx, http.MethodGet, "/v1/package/"+kind+"-"+arch+".json", "", nil)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		return nil, err
	}
	info := &PackageInfo{}
	if err := json.Unmarshal(b, info); err != nil {
		return nil, fmt.Errorf("unexpected answer about the package: %v", err)
	}
	return info, nil
}

// Package downloads the client package the server offers.
func (c *Client) Package(ctx context.Context, kind, arch string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/v1/package/"+kind+"-"+arch, "", nil)
}
