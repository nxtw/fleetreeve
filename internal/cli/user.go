package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/nxtw/fleetreeve/internal/dashboard"
)

// user manages the accounts of the web console:
//
//	fleetreeve user add <name> [-role viewer|operator|admin] -users <file>
//	fleetreeve user role <name> <role> -users <file>
//	fleetreeve user rm <name> -users <file>
//	fleetreeve user list -users <file>
//
// `add` also replaces an existing account's password and role. The
// password is generated and printed once, unless one is piped in.
func user(o *options) error {
	if o.users == "" {
		o.users = os.Getenv("FLEETREEVE_DASHBOARD_USERS")
	}
	if len(o.args) == 0 || o.users == "" {
		return fmt.Errorf("usage: fleetreeve user add <name> [-role viewer|operator|admin] -users <file>\n       fleetreeve user role <name> <role> -users <file>\n       fleetreeve user rm <name> -users <file>\n       fleetreeve user list -users <file>")
	}
	users, err := dashboard.LoadUsers(o.users)
	if err != nil {
		return err
	}
	switch verb := o.args[0]; {
	case verb == "list" && len(o.args) == 1:
		if len(users) == 0 {
			fmt.Printf("No accounts in %s yet. Add one with: fleetreeve user add <name> -users %s\n", o.users, o.users)
		}
		for _, u := range dashboard.SortedUsers(users) {
			fmt.Printf("%-24s %s\n", u.Name, u.Role)
		}
		return nil
	case verb == "rm" && len(o.args) == 2:
		name := o.args[1]
		if _, ok := users[name]; !ok {
			return fmt.Errorf("there is no account %q in %s", name, o.users)
		}
		delete(users, name)
		if err := dashboard.SaveUsers(o.users, users); err != nil {
			return err
		}
		fmt.Printf("Removed %s. If signed in, they are signed out at their next request.\n", name)
		return nil
	case verb == "role" && len(o.args) == 3:
		// Change a role and keep the password.
		name, role := strings.ToLower(o.args[1]), o.args[2]
		u, ok := users[name]
		if !ok {
			return fmt.Errorf("there is no account %q in %s", name, o.users)
		}
		if !dashboard.ValidRole(role) {
			return fmt.Errorf("the role must be viewer, operator or admin")
		}
		u.Role = role
		users[name] = u
		if err := dashboard.SaveUsers(o.users, users); err != nil {
			return err
		}
		fmt.Printf("%s is now %s. If signed in, they sign in again.\n", name, role)
		return nil
	case verb == "add" && len(o.args) == 2:
		name := strings.ToLower(o.args[1])
		if !dashboard.ValidUserName(name) {
			return fmt.Errorf("%q cannot be used as a name: use lower-case letters, digits, dot, dash or underscore", o.args[1])
		}
		role := o.role
		if prev, exists := users[name]; role == "" && exists {
			role = prev.Role // a new password keeps the role
		}
		if role == "" {
			role = dashboard.RoleViewer
		}
		if !dashboard.ValidRole(role) {
			return fmt.Errorf("-role must be viewer, operator or admin")
		}
		// A password piped in is used; otherwise one is generated, so it
		// never has to be typed where others can see it or kept in history.
		password, generated := "", false
		if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			password = strings.TrimRight(line, "\r\n")
		}
		if password == "" {
			password, generated = dashboard.GeneratePassword(), true
		} else if len(password) < dashboard.MinPassword {
			return fmt.Errorf("that password is too short: use at least %d characters", dashboard.MinPassword)
		}
		_, existed := users[name]
		users[name] = dashboard.User{Name: name, Role: role, Hash: dashboard.HashPassword(password)}
		if err := dashboard.SaveUsers(o.users, users); err != nil {
			return err
		}
		what := "Added"
		if existed {
			what = "Updated"
		}
		fmt.Printf("%s %s (%s).\n", what, name, role)
		if generated {
			fmt.Printf("Password: %s\nIt is shown this once and stored only as a hash. Run the same command again for a new one.\n", password)
		}
		return nil
	}
	return fmt.Errorf("usage: fleetreeve user add <name> [-role viewer|operator|admin] | role <name> <role> | rm <name> | list   (with -users <file>)")
}
