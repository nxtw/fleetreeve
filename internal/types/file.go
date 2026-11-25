// Package types implements the built-in resource types.
package types

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/nxtw/fleetreeve/internal/core"
)

func init() { core.Register(fileType{}) }

type fileType struct{}

func (fileType) Name() string { return "file" }
func (fileType) Doc() string {
	return "Manages a file, directory or symlink: existence, content, permissions and ownership."
}

func (fileType) Schema() core.Schema {
	return core.Schema{
		"path":        {Kind: core.String, NameVar: true, Doc: "Absolute path (defaults to the title)."},
		"ensure":      {Kind: core.String, Default: "file", Enum: []string{"file", "directory", "link", "absent"}, Doc: "What should exist at the path."},
		"content":     {Kind: core.String, Doc: "Literal file content."},
		"source":      {Kind: core.String, Doc: "File on the controller to copy, relative to the manifest."},
		"template":    {Kind: core.String, Doc: "Template on the controller to render, relative to the manifest."},
		"content_b64": {Kind: core.String, Internal: true},
		"mode":        {Kind: core.String, Doc: "Permissions in octal, e.g. \"0644\"."},
		"owner":       {Kind: core.String, Doc: "Owning user (name or uid)."},
		"group":       {Kind: core.String, Doc: "Owning group (name or gid)."},
		"target":      {Kind: core.String, Doc: "Link target (ensure: link)."},
		"force":       {Kind: core.Bool, Doc: "Allow replacing or removing a non-empty directory."},
		"sensitive":   {Kind: core.Bool, Doc: "Never show content diffs."},
		"validate":    {Kind: core.String, Doc: "Command that must succeed on the new content before it replaces the file; %s is the candidate path."},
		"purge":       {Kind: core.Bool, Doc: "With ensure: directory: remove what is in the directory that this configuration does not manage. Files and links; whole subdirectories only with force: true."},
		"ignore":      {Kind: core.List, Doc: "With purge: names to leave alone, as patterns (\"*.rpmsave\")."},
		"recurse":     {Kind: core.Bool, Doc: "With purge: also purge inside the subdirectories that are kept."},
		"keep":        {Kind: core.List, Internal: true},
	}
}

func (fileType) Validate(r *core.Resource) error {
	if !filepath.IsAbs(r.Str("path")) {
		return fmt.Errorf("path %q must be absolute", r.Str("path"))
	}
	n := 0
	for _, k := range []string{"content", "source", "template", "content_b64"} {
		if r.Has(k) {
			n++
		}
	}
	ensure := r.Str("ensure")
	if n > 1 {
		return fmt.Errorf("content, source and template are mutually exclusive")
	}
	if n > 0 && ensure != "file" {
		return fmt.Errorf("content cannot be set with ensure: %s", ensure)
	}
	if (ensure == "link") != r.Has("target") {
		return fmt.Errorf("target is required with, and only valid with, ensure: link")
	}
	if m := r.Str("mode"); m != "" {
		if v, err := strconv.ParseUint(m, 8, 32); err != nil || v > 07777 {
			return fmt.Errorf("mode %q is not a valid octal mode", m)
		}
	}
	return validatePurge(r)
}

func (fileType) Compile(r *core.Resource, cc core.CompileCtx) error {
	var data []byte
	switch {
	case r.Has("source"):
		b, err := cc.ReadFile(r.Str("source"))
		if err != nil {
			return fmt.Errorf("source: %v", err)
		}
		data = b
		delete(r.Attrs, "source")
	case r.Has("template"):
		b, err := cc.ReadFile(r.Str("template"))
		if err != nil {
			return fmt.Errorf("template: %v", err)
		}
		s, err := cc.Render(string(b))
		if err != nil {
			return fmt.Errorf("template %s: %v", r.Str("template"), err)
		}
		data = []byte(s)
		delete(r.Attrs, "template")
	default:
		return nil
	}
	if utf8.Valid(data) {
		r.Attrs["content"] = string(data)
	} else {
		r.Attrs["content_b64"] = base64.StdEncoding.EncodeToString(data)
	}
	return nil
}

func (fileType) AutoRequire(r *core.Resource) []string {
	refs := []string{"file[" + filepath.Dir(r.Str("path")) + "]"}
	if o := r.Str("owner"); o != "" {
		refs = append(refs, "user["+o+"]")
	}
	if g := r.Str("group"); g != "" {
		refs = append(refs, "group["+g+"]")
	}
	return refs
}

func (fileType) Absent(r *core.Resource) map[string]any {
	return map[string]any{"path": r.Str("path"), "ensure": "absent"}
}

// maxSnapshot bounds the file content kept for a rollback.
const maxSnapshot = 32 << 20

// Snapshot records what is at the path: nothing, a file with its content,
// mode and owner, a directory, or a link.
func (fileType) Snapshot(_ *core.Ctx, r *core.Resource) (core.Snapshot, error) {
	s, err := snapshotPath(r.Str("path"))
	if err != nil {
		return nil, err
	}
	// What a purge is about to remove is kept too.
	purged, skipped, err := purgedSnapshot(r)
	if err != nil {
		return nil, err
	}
	if len(purged) > 0 {
		s["purged"] = purged
	}
	if skipped > 0 {
		s["purged_not_kept"] = skipped
	}
	return s, nil
}

func (fileType) Restore(ctx *core.Ctx, r *core.Resource, s core.Snapshot) error {
	if err := restorePath(ctx, r.Str("path"), s); err != nil {
		return err
	}
	return restorePurged(ctx, r.Str("path"), s["purged"])
}

func snapshotPath(path string) (core.Snapshot, error) {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return core.Snapshot{"kind": "absent"}, nil
	}
	if err != nil {
		return nil, err
	}
	s := core.Snapshot{"kind": kindOf(st.Mode()), "mode": modeString(st.Mode())}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		s["uid"], s["gid"] = int(sys.Uid), int(sys.Gid)
	}
	switch s["kind"] {
	case "file":
		if st.Size() > maxSnapshot {
			return nil, fmt.Errorf("%s is too large to keep a copy of (%d MB)", path, st.Size()>>20)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		s["content"] = base64.StdEncoding.EncodeToString(b)
	case "link":
		s["target"], _ = os.Readlink(path)
	case "other":
		return nil, fmt.Errorf("%s is not a file, directory or link", path)
	}
	return s, nil
}

func restorePath(ctx *core.Ctx, path string, s core.Snapshot) error {
	kind, _ := s["kind"].(string)
	num := func(k string) int { // numbers come back from JSON as floats
		switch v := s[k].(type) {
		case int:
			return v
		case float64:
			return int(v)
		}
		return -1
	}
	// Whatever is there now and is of another kind has to go first. A
	// directory is only removed when empty: its contents are not ours.
	if st, err := os.Lstat(path); err == nil && kindOf(st.Mode()) != kind {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("cannot restore %s: %v", path, err)
		}
	}
	switch kind {
	case "absent":
		return nil
	case "link":
		os.Remove(path)
		return os.Symlink(fmt.Sprint(s["target"]), path)
	case "directory":
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	case "file":
		data, err := base64.StdEncoding.DecodeString(fmt.Sprint(s["content"]))
		if err != nil {
			return fmt.Errorf("the kept copy of %s is damaged: %v", path, err)
		}
		if err := atomicWrite(ctx, path, data, "", 0, false); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown snapshot kind %q", kind)
	}
	if m, ok := s["mode"].(string); ok {
		if v, err := strconv.ParseUint(m, 8, 32); err == nil {
			if err := os.Chmod(path, fileMode(uint32(v))); err != nil {
				return err
			}
		}
	}
	if uid, gid := num("uid"), num("gid"); uid >= 0 && gid >= 0 {
		_ = os.Chown(path, uid, gid) // best effort when not root
	}
	return nil
}

func desiredContent(r *core.Resource) ([]byte, bool) {
	if r.Has("content") {
		return []byte(r.Str("content")), true
	}
	if r.Has("content_b64") {
		b, _ := base64.StdEncoding.DecodeString(r.Str("content_b64"))
		return b, true
	}
	return nil, false
}

func kindOf(m os.FileMode) string {
	switch {
	case m.IsRegular():
		return "file"
	case m.IsDir():
		return "directory"
	case m&os.ModeSymlink != 0:
		return "link"
	}
	return "other"
}

func digest(b []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))[:19]
}

func (fileType) Check(ctx *core.Ctx, r *core.Resource) ([]core.Change, error) {
	path, ensure := r.Str("path"), r.Str("ensure")
	st, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	cur := "absent"
	if err == nil {
		cur = kindOf(st.Mode())
	}
	want, hasContent := desiredContent(r)
	contentDiff := func(old []byte) string {
		if r.Bool("sensitive") {
			return "(sensitive content not shown)\n"
		}
		return core.Diff(string(old), string(want))
	}
	if cur != ensure {
		ch := core.Change{Prop: "ensure", From: cur, To: ensure}
		if ensure == "file" && hasContent {
			ch.Diff = contentDiff(nil)
		}
		if ensure == "link" {
			ch.To = "link -> " + r.Str("target")
		}
		return []core.Change{ch}, nil
	}
	var changes []core.Change
	switch ensure {
	case "absent":
		return nil, nil
	case "link":
		if t, _ := os.Readlink(path); t != r.Str("target") {
			changes = append(changes, core.Change{Prop: "target", From: t, To: r.Str("target")})
		}
		return changes, nil
	case "file":
		if hasContent {
			old, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(old, want) {
				changes = append(changes, core.Change{Prop: "content", From: digest(old), To: digest(want), Diff: contentDiff(old)})
			}
		}
	}
	if m := r.Str("mode"); m != "" {
		if cm := modeString(st.Mode()); cm != normMode(m) {
			changes = append(changes, core.Change{Prop: "mode", From: cm, To: normMode(m)})
		}
	}
	sys := st.Sys().(*syscall.Stat_t)
	if o := r.Str("owner"); o != "" {
		if id, ok := lookupID(o, false); !ok || id != int(sys.Uid) {
			changes = append(changes, core.Change{Prop: "owner", From: idName(int(sys.Uid), false), To: o})
		}
	}
	if g := r.Str("group"); g != "" {
		if id, ok := lookupID(g, true); !ok || id != int(sys.Gid) {
			changes = append(changes, core.Change{Prop: "group", From: idName(int(sys.Gid), true), To: g})
		}
	}
	if ensure == "directory" {
		gone, err := purgeChange(r)
		if err != nil {
			return nil, err
		}
		if gone != nil {
			changes = append(changes, *gone)
		}
	}
	return changes, nil
}

func (fileType) Apply(ctx *core.Ctx, r *core.Resource, changes []core.Change) error {
	path, ensure := r.Str("path"), r.Str("ensure")
	has := map[string]bool{}
	for _, c := range changes {
		has[c.Prop] = true
	}
	mode := os.FileMode(0)
	if m := r.Str("mode"); m != "" {
		v, _ := strconv.ParseUint(m, 8, 32)
		mode = fileMode(uint32(v))
	}
	if has["ensure"] || has["target"] {
		if err := clearPath(path, ensure, r.Bool("force")); err != nil {
			return err
		}
	}
	switch {
	case ensure == "absent":
		return nil
	case ensure == "link":
		if has["ensure"] || has["target"] {
			return os.Symlink(r.Str("target"), path)
		}
		return nil
	case ensure == "directory" && has["ensure"]:
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	case ensure == "file" && (has["ensure"] || has["content"]):
		data, _ := desiredContent(r)
		if err := atomicWrite(ctx, path, data, r.Str("validate"), mode, r.Has("mode")); err != nil {
			return err
		}
	}
	if has["purge"] {
		if err := purge(r); err != nil {
			return err
		}
	}
	// Attributes are (re)asserted against the final object, which also
	// covers files that were just created.
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if r.Has("mode") && modeString(st.Mode()) != modeString(mode) {
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	uid, gid := -1, -1
	if o := r.Str("owner"); o != "" {
		id, ok := lookupID(o, false)
		if !ok {
			return fmt.Errorf("owner: unknown user %q", o)
		}
		uid = id
	}
	if g := r.Str("group"); g != "" {
		id, ok := lookupID(g, true)
		if !ok {
			return fmt.Errorf("group: unknown group %q", g)
		}
		gid = id
	}
	sys := st.Sys().(*syscall.Stat_t)
	if (uid >= 0 && uid != int(sys.Uid)) || (gid >= 0 && gid != int(sys.Gid)) {
		return os.Chown(path, uid, gid)
	}
	return nil
}

// clearPath removes whatever is at path when it is in the way of ensure.
func clearPath(path, ensure string, force bool) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.IsDir() {
		if ensure == "directory" {
			return nil
		}
		if force {
			return os.RemoveAll(path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("%s is a non-empty directory; set force: true to remove it", path)
		}
		return nil
	}
	if ensure == "file" && st.Mode().IsRegular() {
		return nil
	}
	return os.Remove(path)
}

// atomicWrite replaces path via a temp file in the same directory, keeping
// the mode and ownership of an existing file. When setMode is true the
// file appears with exactly that mode, so a file that must be private (or,
// like a sudoers drop-in, must have one specific mode) never exists with
// another, not even for a moment.
func atomicWrite(ctx *core.Ctx, path string, data []byte, validate string, want os.FileMode, setMode bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fleetreeve-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
		sys := st.Sys().(*syscall.Stat_t)
		_ = os.Chown(name, int(sys.Uid), int(sys.Gid))
	}
	if setMode {
		mode = want
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	if validate != "" {
		res := ctx.Cmd("/bin/sh", "-c", strings.ReplaceAll(validate, "%s", shQuote(name)))
		if !res.OK() {
			return res.Error("validate")
		}
	}
	return os.Rename(name, path)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// fileMode converts octal permission bits (as in chmod) to an os.FileMode.
func fileMode(v uint32) os.FileMode {
	m := os.FileMode(v & 0o777)
	if v&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if v&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if v&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

func modeString(m os.FileMode) string {
	v := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		v |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		v |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		v |= 0o1000
	}
	return fmt.Sprintf("%04o", v)
}

func normMode(s string) string {
	v, _ := strconv.ParseUint(s, 8, 32)
	return fmt.Sprintf("%04o", v)
}

// lookupID resolves a user or group given by name or numeric id.
func lookupID(name string, group bool) (int, bool) {
	if id, err := strconv.Atoi(name); err == nil {
		return id, true
	}
	var s string
	if group {
		g, err := user.LookupGroup(name)
		if err != nil {
			return 0, false
		}
		s = g.Gid
	} else {
		u, err := user.Lookup(name)
		if err != nil {
			return 0, false
		}
		s = u.Uid
	}
	id, err := strconv.Atoi(s)
	return id, err == nil
}

func idName(id int, group bool) string {
	s := strconv.Itoa(id)
	if group {
		if g, err := user.LookupGroupId(s); err == nil {
			return g.Name
		}
	} else if u, err := user.LookupId(s); err == nil {
		return u.Username
	}
	return s
}
