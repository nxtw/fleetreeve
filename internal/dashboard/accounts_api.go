package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

var errSelf = errors.New("you cannot remove your own account; ask another admin")

// Account management in the console. Only admins may use it, only from
// the console's own pages, and every change is logged with who made it.
// Passwords are never chosen by an admin: a new or reset account gets a
// generated one, shown once to the admin to pass on, and its owner then
// replaces it under "Change password".

// manager checks that a request comes from a signed-in admin, through the
// console itself, and returns who that is.
func (d *Dashboard) manager(w http.ResponseWriter, r *http.Request) (User, bool) {
	u, ok := d.identify(r)
	switch {
	case !ok || u.Role != RoleAdmin:
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "managing accounts needs the admin role"})
	case r.Method != http.MethodGet && r.Header.Get("X-Fleetreeve-Console") != "1":
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "request refused: it did not come from the console"})
	default:
		return u, true
	}
	return User{}, false
}

func fail(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func (d *Dashboard) listUsers(w http.ResponseWriter, r *http.Request) {
	me, ok := d.manager(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": d.Users.List(), "you": me.Name})
}

func (d *Dashboard) addUser(w http.ResponseWriter, r *http.Request) {
	me, ok := d.manager(w, r)
	if !ok {
		return
	}
	var req struct{ Name, Role string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		fail(w, err)
		return
	}
	req.Name = strings.ToLower(strings.TrimSpace(req.Name)) // as it is stored
	password, err := d.Users.Add(req.Name, req.Role)
	if err != nil {
		fail(w, err)
		return
	}
	d.Log.Info("account added", "account", req.Name, "role", req.Role, "by", me.Name, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "role": req.Role, "password": password})
}

// changeUser sets a role, or resets the password, of an account.
func (d *Dashboard) changeUser(w http.ResponseWriter, r *http.Request) {
	me, ok := d.manager(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var req struct {
		Role  string
		Reset bool
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		fail(w, err)
		return
	}
	out := map[string]string{"name": name}
	if req.Role != "" {
		if err := d.Users.SetRole(name, req.Role); err != nil {
			fail(w, err)
			return
		}
		d.Log.Info("account role changed", "account", name, "role", req.Role, "by", me.Name, "remote", r.RemoteAddr)
		out["role"] = req.Role
	}
	if req.Reset {
		password, err := d.Users.ResetPassword(name)
		if err != nil {
			fail(w, err)
			return
		}
		d.Log.Info("account password reset", "account", name, "by", me.Name, "remote", r.RemoteAddr)
		out["password"] = password
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Dashboard) removeUser(w http.ResponseWriter, r *http.Request) {
	me, ok := d.manager(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if name == me.Name {
		fail(w, errSelf)
		return
	}
	if err := d.Users.Remove(name); err != nil {
		fail(w, err)
		return
	}
	d.Log.Info("account removed", "account", name, "by", me.Name, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}

// changePassword lets anyone signed in replace their own password.
func (d *Dashboard) changePassword(w http.ResponseWriter, r *http.Request) {
	me, ok := d.identify(r)
	if !ok || r.Header.Get("X-Fleetreeve-Console") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sign in to change your password"})
		return
	}
	var req struct{ Current, New string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		fail(w, err)
		return
	}
	keep := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		keep = c.Value
	}
	if err := d.Users.ChangePassword(me.Name, req.Current, req.New, keep); err != nil {
		d.Log.Warn("password change refused", "account", me.Name, "reason", err.Error(), "remote", r.RemoteAddr)
		fail(w, err)
		return
	}
	d.Log.Info("password changed", "account", me.Name, "remote", r.RemoteAddr)
	w.WriteHeader(http.StatusNoContent)
}
