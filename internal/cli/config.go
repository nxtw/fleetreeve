package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A settings file supplies defaults for command-line options, so that
// values which never change in a project - the server, its secret and CA,
// the inventory - are written down once instead of typed on every command.
//
// fleetreeve uses .fleetreeve.yaml from the current directory or the nearest parent
// that has one, else ~/.config/fleetreeve/config.yaml. $FLEETREEVE_CONFIG names
// another file, or "none" to use no file. Options given on the command
// line always win.
//
//	server: https://fleetreeve.example.com:8140     # top level: every command
//	secret-file: server.secret
//	ca: server.crt
//	enroll:                                     # a command's name: only that command
//	  inventory: inventory.yaml
//	  rpm: dist
//
// Keys are the long option names; `inventory` and `limit` stand for -i
// and -l. Relative paths are taken from the directory of the file.

// pathSettings are options whose value is a file or directory.
var pathSettings = map[string]bool{
	"i": true, "secret-file": true, "ca": true, "rpm": true, "report": true, "reports": true,
	"token-file": true, "tls-cert": true, "tls-key": true, "facts": true, "hosts": true, "actions": true, "users": true,
}

func findConfig() string {
	switch env := os.Getenv("FLEETREEVE_CONFIG"); env {
	case "":
	case "none":
		return ""
	default:
		return env
	}
	if dir, err := os.Getwd(); err == nil {
		for {
			if p := filepath.Join(dir, ".fleetreeve.yaml"); isRegular(p) {
				return p
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".config", "fleetreeve", "config.yaml"); isRegular(p) {
			return p
		}
	}
	return ""
}

func isRegular(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// applyConfig sets option defaults from the settings file: first the
// top-level settings, then those in the section named after the command.
// It returns the file used, if any.
func applyConfig(fs *flag.FlagSet, cmd string) (string, error) {
	path := findConfig()
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("%s: %v", path, err)
	}
	set := func(section string, settings map[string]any) error {
		keys := make([]string, 0, len(settings))
		for k := range settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			v := settings[key]
			if _, isSection := v.(map[string]any); isSection && section == "" {
				continue
			}
			name := strings.ReplaceAll(key, "_", "-")
			switch name {
			case "inventory":
				name = "i"
			case "limit":
				name = "l"
			}
			where := path
			if section != "" {
				where += ": " + section
			}
			if fs.Lookup(name) == nil || name == "e" {
				return fmt.Errorf("%s: unknown setting %q", where, key)
			}
			val := fmt.Sprint(v)
			if pathSettings[name] && val != "" && !filepath.IsAbs(val) {
				val = filepath.Join(filepath.Dir(path), val)
			}
			if err := fs.Set(name, val); err != nil {
				return fmt.Errorf("%s: %s: %v", where, key, err)
			}
		}
		return nil
	}
	if err := set("", doc); err != nil {
		return "", err
	}
	if section, ok := doc[cmd].(map[string]any); ok {
		if err := set(cmd, section); err != nil {
			return "", err
		}
	}
	return path, nil
}
