package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The directory belongs to someone else, who can put a link where root is
// about to write. Root's write must land in the file of that name or
// nowhere - never in what a link points to.
func TestWriteOwnedDoesNotFollowLinks(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "shadow")
	os.WriteFile(victim, []byte("root's own file\n"), 0o600)
	token := filepath.Join(dir, "token")

	// The ordinary cases: a new file, and an existing one.
	if err := writeOwned(token, []byte("first\n"), 0o600, -1, -1); err != nil {
		t.Fatal(err)
	}
	os.Chmod(token, 0o644)
	if err := writeOwned(token, []byte("second\n"), 0o600, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if b, _ := readOwned(token); string(b) != "second\n" {
		t.Errorf("content %q", b)
	}
	if st, _ := os.Stat(token); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}

	// A symbolic link in its place: refused for writing and for reading,
	// and what it points to is as it was.
	os.Remove(token)
	if err := os.Symlink(victim, token); err != nil {
		t.Fatal(err)
	}
	err := writeOwned(token, []byte("a new token\n"), 0o600, -1, -1)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("writing through a link: %v", err)
	}
	if _, err := readOwned(token); err == nil {
		t.Error("reading through a link was allowed")
	}
	if b, _ := os.ReadFile(victim); string(b) != "root's own file\n" {
		t.Errorf("the file the link points to now holds %q", b)
	}
	// A link to nothing: no file is made where it points either.
	os.Remove(token)
	nowhere := filepath.Join(t.TempDir(), "made-by-root")
	os.Symlink(nowhere, token)
	if err := writeOwned(token, []byte("x"), 0o600, -1, -1); err == nil {
		t.Error("writing through a dangling link was allowed")
	}
	if _, err := os.Lstat(nowhere); err == nil {
		t.Error("a file was created where the link points")
	}

	// A second name for another file (a hard link): not written through.
	os.Remove(token)
	if err := os.Link(victim, token); err != nil {
		t.Skipf("cannot make a hard link here: %v", err)
	}
	if err := writeOwned(token, []byte("a new token\n"), 0o600, -1, -1); err == nil || !strings.Contains(err.Error(), "more than one name") {
		t.Errorf("writing through a hard link: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "root's own file\n" {
		t.Errorf("the hard-linked file now holds %q", b)
	}

	// Not a file at all.
	os.Remove(token)
	os.Mkdir(token, 0o755)
	if err := writeOwned(token, []byte("x"), 0o600, -1, -1); err == nil {
		t.Error("writing to a directory was allowed")
	}
}
