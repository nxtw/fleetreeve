// Package state records, on each node, what fleetreeve manages there. It is
// what lets fleetreeve notice that a resource was dropped from the manifest (and
// undo it), and tell drift on the node apart from a change in the manifest.
//
// State lives on the node it describes rather than on the controller, so
// every controller (laptops, CI) sees the same state and a lock on the node
// serialises their runs. One file is kept per manifest name and host.
package state

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/nxtw/fleetreeve/internal/core"
)

const Version = 1

// Entry is what fleetreeve remembers about one managed resource.
type Entry struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	// Digest identifies the desired state last applied. Attributes
	// themselves are not stored, so file contents and secrets stay out.
	Digest string `json:"digest"`
	// Created is true when fleetreeve brought the resource into existence, as
	// opposed to adopting something that was already there. Only created
	// resources are removed when they leave the manifest.
	Created bool `json:"created"`
	// Absent declares how to undo the resource, if its type can.
	Absent  map[string]any `json:"absent,omitempty"`
	Order   int            `json:"order"`             // position in the last run
	Changed string         `json:"changed,omitempty"` // when fleetreeve last changed it
	// Hold is set (to the time) by a manual `fleetreeve rollback`: the resource
	// is left alone until released, so the next run does not simply redo
	// the change that was rolled back.
	Hold string `json:"hold,omitempty"`
}

// File is the state of one manifest on one node.
type File struct {
	Version   int               `json:"version"`
	Name      string            `json:"name"`
	Serial    int               `json:"serial"` // incremented on every write
	Updated   string            `json:"updated,omitempty"`
	Resources map[string]*Entry `json:"resources"`
}

var (
	validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	validKey  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(@[A-Za-z0-9._-]+)?$`)
	unsafe    = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

// ValidName reports whether a manifest name can be used for state.
func ValidName(name string) bool { return validName.MatchString(name) }

// Key names the state of one manifest as applied to one inventory host.
// Including the host keeps hosts apart that share a machine (several
// `connection: local` hosts, containers with a shared state directory).
func Key(name, host string) string {
	if host == "" {
		return name
	}
	return name + "@" + unsafe.ReplaceAllString(host, "_")
}

// Dir is where state is kept: $FLEETREEVE_STATE_DIR, /var/lib/fleetreeve for root,
// otherwise the user's XDG state directory.
func Dir() string {
	if d := os.Getenv("FLEETREEVE_STATE_DIR"); d != "" {
		return d
	}
	if os.Getuid() == 0 {
		return "/var/lib/fleetreeve/state"
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "fleetreeve")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "fleetreeve")
}

func path(name string) (string, error) {
	if !validKey.MatchString(name) {
		return "", fmt.Errorf("invalid state name %q", name)
	}
	return filepath.Join(Dir(), name+".json"), nil
}

// Load reads state without locking. Missing state is empty state.
func Load(name string) (*File, error) {
	p, err := path(name)
	if err != nil {
		return nil, err
	}
	f := &File{Version: Version, Name: name, Resources: map[string]*Entry{}}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("state %s is corrupt: %v", p, err)
	}
	if f.Version > Version {
		return nil, fmt.Errorf("state %s was written by a newer fleetreeve (format %d)", p, f.Version)
	}
	if f.Resources == nil {
		f.Resources = map[string]*Entry{}
	}
	return f, nil
}

// Store is state opened for writing; it holds an exclusive lock.
type Store struct {
	*File
	path string
	lock *os.File
}

// Open locks and loads the state. It fails immediately when another run
// holds the lock.
func Open(name string) (*Store, error) {
	p, err := path(name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(p + ".lock")
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("state %q is locked by another fleetreeve run (%s)", name, string(holder))
		}
		return nil, err
	}
	lock.Truncate(0)
	fmt.Fprintf(lock, "pid %d since %s", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	f, err := Load(name)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &Store{File: f, path: p, lock: lock}, nil
}

// Save writes the state atomically.
func (s *Store) Save() error {
	s.Serial++
	s.Updated = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(s.File, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Close releases the lock.
func (s *Store) Close() {
	s.lock.Truncate(0)
	s.lock.Close()
}

// Digest fingerprints the desired state of a resource.
func Digest(r *core.Resource) string {
	b, _ := json.Marshal(r.Attrs) // map keys are sorted, so this is stable
	return fmt.Sprintf("%x", sha256.Sum256(b))[:32]
}
