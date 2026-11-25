package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nxtw/fleetreeve/internal/compile"
)

// Modules are kept in ./modules, next to the manifests that use them, and
// are meant to be checked in with them: what a server compiles from is
// then exactly what was reviewed. Two files record where they came from:
//
//	modules.yaml   what the project wants: name, source, version
//	modules.lock   what it got: the exact commit and a checksum of the files
//
// `fleetreeve module add` fetches a module and records it; `install` fetches
// whatever is listed but missing, at exactly the locked content; `verify`
// tells whether anything in ./modules differs from what was locked.

type moduleReq struct {
	Name    string `yaml:"name"`
	Source  string `yaml:"source"`            // git URL, or a local directory
	Version string `yaml:"version,omitempty"` // git tag or branch
	Path    string `yaml:"path,omitempty"`    // directory of the module inside the source
}

type moduleLock struct {
	Name    string `yaml:"name"`
	Source  string `yaml:"source"`
	Version string `yaml:"version,omitempty"`
	Commit  string `yaml:"commit,omitempty"`
	SHA256  string `yaml:"sha256"`
}

const (
	reqFile  = "modules.yaml"
	lockFile = "modules.lock"
	modDir   = "modules"
)

func readYAML(path string, into any) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

func writeYAML(path, header string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(header), b...), 0o644)
}

// treeHash is a checksum of a module's files: names and contents.
func treeHash(dir string) (string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		fmt.Fprintf(h, "%s\x00", filepath.ToSlash(rel))
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%d\x00", len(b))
		h.Write(b)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
		return nil // links and the like have no place in a module
	})
}

// fetch gets a module's files into a fresh directory under ./modules and
// returns that directory and, for git, the commit.
func fetch(req moduleReq, commit string) (dir, got string, err error) {
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp(modDir, ".fetch-")
	if err != nil {
		return "", "", err
	}
	src := req.Source
	st, serr := os.Stat(req.Source)
	_, gitErr := os.Stat(filepath.Join(req.Source, ".git"))
	if serr == nil && st.IsDir() && gitErr != nil {
		// A plain directory on this machine: copied as it is. (One that
		// is a git repository is fetched like any other, so that the
		// version asked for is the version got, and the commit is locked.)
		if req.Version != "" {
			os.RemoveAll(tmp)
			return "", "", fmt.Errorf("%s is not a git repository, so it has no version %s", req.Source, req.Version)
		}
	} else {
		clone := filepath.Join(tmp, "clone")
		args := []string{"clone", "--quiet"}
		if commit == "" {
			args = append(args, "--depth", "1")
			if req.Version != "" {
				args = append(args, "--branch", req.Version)
			}
		}
		if out, gerr := exec.Command("git", append(args, req.Source, clone)...).CombinedOutput(); gerr != nil {
			os.RemoveAll(tmp)
			return "", "", fmt.Errorf("fetching %s: %s", req.Source, strings.TrimSpace(string(out)))
		}
		if commit != "" {
			if out, gerr := exec.Command("git", "-C", clone, "checkout", "--quiet", commit).CombinedOutput(); gerr != nil {
				os.RemoveAll(tmp)
				return "", "", fmt.Errorf("%s no longer has commit %s: %s", req.Source, commit, strings.TrimSpace(string(out)))
			}
		}
		head, _ := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output()
		got, src = strings.TrimSpace(string(head)), clone
	}
	out := filepath.Join(tmp, "module")
	if err := copyTree(filepath.Join(src, req.Path), out); err != nil {
		os.RemoveAll(tmp)
		return "", "", err
	}
	return out, got, nil
}

// place moves fetched files to ./modules/<name>, replacing what is there.
func place(fetched, name string) error {
	dest := filepath.Join(modDir, name)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.Rename(fetched, dest); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Dir(fetched))
}

func module(o *options) error {
	usage := fmt.Errorf("usage: fleetreeve module add <git-url|directory> [-version <tag|branch>] [-path <dir in source>]\n" +
		"       fleetreeve module install      fetch what modules.yaml lists and ./modules lacks\n" +
		"       fleetreeve module verify       check ./modules against modules.lock\n" +
		"       fleetreeve module list         installed modules, their roles and types\n" +
		"       fleetreeve module init <namespace.name>   start a new module in ./modules")
	if len(o.args) == 0 {
		return usage
	}
	var reqs struct {
		Modules []moduleReq `yaml:"modules"`
	}
	var locks struct {
		Modules []moduleLock `yaml:"modules"`
	}
	if err := readYAML(reqFile, &reqs); err != nil {
		return err
	}
	if err := readYAML(lockFile, &locks); err != nil {
		return err
	}
	locked := func(name string) *moduleLock {
		for i := range locks.Modules {
			if locks.Modules[i].Name == name {
				return &locks.Modules[i]
			}
		}
		return nil
	}
	save := func() error {
		sort.Slice(reqs.Modules, func(a, b int) bool { return reqs.Modules[a].Name < reqs.Modules[b].Name })
		sort.Slice(locks.Modules, func(a, b int) bool { return locks.Modules[a].Name < locks.Modules[b].Name })
		if err := writeYAML(reqFile, "# Modules this project uses. Managed with `fleetreeve module add`.\n", reqs); err != nil {
			return err
		}
		return writeYAML(lockFile, "# Exactly what was fetched for modules.yaml. Do not edit; `fleetreeve module verify` checks ./modules against it.\n", locks)
	}

	switch verb := o.args[0]; {
	case verb == "list" && len(o.args) == 1:
		mods, err := compile.LoadModules(compile.ModulePath("site.yaml"))
		if err != nil {
			return err
		}
		names := make([]string, 0, len(mods))
		for n := range mods {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			fmt.Println("No modules installed. Add one with: fleetreeve module add <git-url|directory>")
		}
		for _, n := range names {
			m := mods[n]
			fmt.Printf("%s %s  %s\n", m.Name, m.Version, m.Description)
			fmt.Printf("  in %s\n", m.Dir)
			for _, r := range m.Roles {
				use := "- module: " + m.Name
				if r != "main" {
					use = fmt.Sprintf("- {module: %s, role: %s}", m.Name, r)
				}
				fmt.Printf("  role  %-28s include:  %s\n", r, use)
			}
			tnames := make([]string, 0, len(m.Types))
			for t := range m.Types {
				tnames = append(tnames, t)
			}
			sort.Strings(tnames)
			for _, t := range tnames {
				fmt.Printf("  type  %-28s %s; %s\n", t, m.Types[t].Kind(), m.Types[t].Doc)
			}
		}
		return nil

	case verb == "init" && len(o.args) == 2:
		name := o.args[1]
		if !compile.ValidModuleName(name) {
			return fmt.Errorf("%q: a module is called namespace.name, in lower case (for example acme.nginx)", name)
		}
		dir := filepath.Join(modDir, name)
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("%s already exists", dir)
		}
		files := map[string]string{
			"module.yaml":      "name: " + name + "\nversion: 0.1.0\ndescription: What this module manages, in one line.\n",
			"README.md":        "# " + name + "\n\nWhat it manages, the roles and types it offers, and the variables they read.\n",
			"roles/main.yaml":  "# Included with:   - module: " + name + "\n# Values come from the including project's data; give defaults here.\nvars:\n  example_setting: default\n\nresources: []\n",
			"types/thing.yaml": "# Used as:   - " + name + ".thing: <title>\n# A composite type: a parameterised bundle of other resources. `title`\n# and the parameters are variables in them. (For a type implemented by a\n# program instead, replace `resources` with  script: thing.sh  - see\n# docs/modules.md in the fleetreeve documentation.)\ndescription: One thing.\nparams:\n  content: {default: \"\", description: What to put in it.}\nresources:\n  - file: \"/tmp/{{ .title }}\"\n    content: \"{{ .content }}\"\n",
		}
		for rel, body := range files {
			p := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				return err
			}
		}
		fmt.Printf("Started %s in %s. `fleetreeve module list` shows what it offers.\n", name, dir)
		return nil

	case verb == "add" && len(o.args) == 2:
		req := moduleReq{Source: o.args[1], Version: o.modVersion, Path: o.modPath}
		fetched, commit, err := fetch(req, "")
		if err != nil {
			return err
		}
		fetched = renamedForLoad(fetched)
		m, err := compile.LoadModule(fetched)
		if err != nil {
			os.RemoveAll(filepath.Dir(fetched))
			return fmt.Errorf("%s does not hold a module: %v", req.Source, err)
		}
		req.Name = m.Name
		sum, err := treeHash(fetched)
		if err != nil {
			return err
		}
		if err := place(fetched, m.Name); err != nil {
			return err
		}
		kept := reqs.Modules[:0]
		for _, r := range reqs.Modules {
			if r.Name != m.Name {
				kept = append(kept, r)
			}
		}
		reqs.Modules = append(kept, req)
		keptLocks := locks.Modules[:0]
		for _, l := range locks.Modules {
			if l.Name != m.Name {
				keptLocks = append(keptLocks, l)
			}
		}
		locks.Modules = append(keptLocks, moduleLock{Name: m.Name, Source: req.Source, Version: req.Version, Commit: commit, SHA256: sum})
		if err := save(); err != nil {
			return err
		}
		fmt.Printf("Added %s %s to %s (%d roles, %d types). Recorded in %s and %s.\n", m.Name, m.Version, filepath.Join(modDir, m.Name), len(m.Roles), len(m.Types), reqFile, lockFile)
		fmt.Println("A module's roles and scripts run as root on your hosts: read what you add, as you would any code.")
		return nil

	case verb == "install" && len(o.args) == 1:
		if len(reqs.Modules) == 0 {
			fmt.Printf("%s lists no modules.\n", reqFile)
			return nil
		}
		for _, req := range reqs.Modules {
			l := locked(req.Name)
			dest := filepath.Join(modDir, req.Name)
			if have, err := treeHash(dest); err == nil && l != nil && have == l.SHA256 {
				fmt.Printf("%s: already there, as locked\n", req.Name)
				continue
			}
			commit := ""
			if l != nil {
				commit = l.Commit
			}
			fetched, got, err := fetch(req, commit)
			if err != nil {
				return fmt.Errorf("%s: %v", req.Name, err)
			}
			sum, err := treeHash(fetched)
			if err != nil {
				return err
			}
			if l != nil && sum != l.SHA256 {
				os.RemoveAll(filepath.Dir(fetched))
				return fmt.Errorf("%s: what %s holds now is not what was locked (checksum %s…, locked %s…). If the change is intended, run `fleetreeve module add` again", req.Name, req.Source, sum[:12], l.SHA256[:12])
			}
			if err := place(fetched, req.Name); err != nil {
				return err
			}
			if l == nil {
				locks.Modules = append(locks.Modules, moduleLock{Name: req.Name, Source: req.Source, Version: req.Version, Commit: got, SHA256: sum})
			}
			fmt.Printf("%s: installed\n", req.Name)
		}
		return save()

	case verb == "verify" && len(o.args) == 1:
		bad := 0
		for _, l := range locks.Modules {
			have, err := treeHash(filepath.Join(modDir, l.Name))
			switch {
			case err != nil:
				fmt.Printf("%s: missing (fleetreeve module install fetches it)\n", l.Name)
				bad++
			case have != l.SHA256:
				fmt.Printf("%s: differs from what was locked\n", l.Name)
				bad++
			default:
				fmt.Printf("%s: as locked\n", l.Name)
			}
		}
		entries, _ := os.ReadDir(modDir)
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && locked(e.Name()) == nil {
				fmt.Printf("%s: not in %s (a module of your own, or added by hand)\n", e.Name(), lockFile)
			}
		}
		if bad > 0 {
			return fmt.Errorf("%d module(s) are not as locked", bad)
		}
		return nil
	}
	return usage
}

// renamedForLoad gives fetched files the directory name their module
// declares, which loading requires, and returns that directory.
func renamedForLoad(fetched string) string {
	b, err := os.ReadFile(filepath.Join(fetched, "module.yaml"))
	if err != nil {
		return fetched
	}
	var m struct {
		Name string `yaml:"name"`
	}
	if yaml.Unmarshal(b, &m) != nil || !compile.ValidModuleName(m.Name) {
		return fetched
	}
	named := filepath.Join(filepath.Dir(fetched), m.Name)
	if os.Rename(fetched, named) != nil {
		return fetched
	}
	return named
}
