package dashboard

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Accounts for the console. Each person has their own name, password and
// role:
//
//	viewer    may look
//	operator  may also ask hosts to roll resources back (if the console
//	          was given the operator token at all)
//	admin     may also manage the accounts, in the console itself
//
// They live in a YAML file managed with `fleetreeve user`; passwords are stored
// as salted PBKDF2-SHA256 hashes. The file is re-read when it changes, so
// adding or removing someone needs no restart.

const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"

	// MinPassword is the least a chosen password must have.
	MinPassword = 12

	pbkdfIterations = 600_000
)

// User is one account.
type User struct {
	Name string `yaml:"-"`
	Role string `yaml:"role"`
	Hash string `yaml:"password"`
}

var userName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidRole reports whether a role exists.
func ValidRole(role string) bool {
	return role == RoleViewer || role == RoleOperator || role == RoleAdmin
}

// MayAct reports whether a role may ask for rollbacks.
func MayAct(role string) bool { return role == RoleOperator || role == RoleAdmin }

// ValidUserName reports whether a name can be used for an account.
func ValidUserName(name string) bool { return userName.MatchString(name) }

// HashPassword returns the stored form of a password.
func HashPassword(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, password, salt, pbkdfIterations, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdfIterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// CheckPassword compares a password with a stored hash.
func CheckPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	salt, err2 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err3 := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || err2 != nil || err3 != nil || iter < 1000 || iter > 10_000_000 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// GeneratePassword returns a random password that is easy to copy.
func GeneratePassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, 20)
	rand.Read(raw)
	out := make([]byte, len(raw))
	for i, b := range raw {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out[:5]) + "-" + string(out[5:10]) + "-" + string(out[10:15]) + "-" + string(out[15:])
}

type usersFile struct {
	Users map[string]User `yaml:"users"`
}

// LoadUsers reads a users file. A missing file holds no users.
func LoadUsers(path string) (map[string]User, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]User{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f usersFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	out := map[string]User{}
	for name, u := range f.Users {
		if !ValidUserName(name) || !ValidRole(u.Role) {
			return nil, fmt.Errorf("%s: user %q needs a valid name and the role viewer, operator or admin", path, name)
		}
		u.Name = name
		out[name] = u
	}
	return out, nil
}

// SaveUsers writes a users file atomically. It is readable by its owner
// only. A new file takes the owner of its directory, so that a console
// running as another user (in a container) can be given the directory.
func SaveUsers(path string, users map[string]User) error {
	b, err := yaml.Marshal(usersFile{Users: users})
	if err != nil {
		return err
	}
	header := "# Accounts of the fleetreeve console. Managed with `fleetreeve user`; passwords are hashes.\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), b...), 0o600); err != nil {
		return err
	}
	owner := path
	if _, err := os.Stat(path); err != nil {
		owner = filepath.Dir(path)
	}
	if st, err := os.Stat(owner); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			_ = os.Chown(tmp, int(sys.Uid), int(sys.Gid)) // only root can; otherwise it stays ours
		}
	}
	return os.Rename(tmp, path)
}

// SortedUsers lists users by name.
func SortedUsers(users map[string]User) []User {
	out := make([]User, 0, len(users))
	for _, u := range users {
		out = append(out, u)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// ---- the running console's view of its accounts ----

// Accounts verifies logins and keeps sessions.
type Accounts struct {
	path   string // users file; "" for none
	static *User  // from FLEETREEVE_DASHBOARD_AUTH: one account given in the environment

	mu       sync.Mutex
	users    map[string]User
	mtime    time.Time
	sessions map[string]session
	fails    map[string]*lockout
	decoy    string // hash checked for unknown names, so they take as long as known ones
}

type session struct {
	user    User
	expires time.Time
}

type lockout struct {
	count int
	until time.Time
	last  time.Time
}

const (
	sessionLife  = 12 * time.Hour
	maxFailures  = 5
	lockDuration = 5 * time.Minute
)

// NewAccounts prepares the console's accounts from a users file and/or a
// single "name:password" given in the environment (which gets the operator
// role). It returns nil when neither is set: the console is then open.
func NewAccounts(usersPath, envAuth string) (*Accounts, error) {
	if usersPath == "" && envAuth == "" {
		return nil, nil
	}
	a := &Accounts{path: usersPath, users: map[string]User{}, sessions: map[string]session{}, fails: map[string]*lockout{}}
	a.decoy = HashPassword(GeneratePassword())
	if envAuth != "" {
		name, pw, ok := strings.Cut(envAuth, ":")
		if !ok || name == "" || pw == "" {
			return nil, fmt.Errorf("FLEETREEVE_DASHBOARD_AUTH must be name:password")
		}
		a.static = &User{Name: name, Role: RoleOperator, Hash: HashPassword(pw)}
	}
	if usersPath != "" {
		if _, err := LoadUsers(usersPath); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// lookup returns an account by name, re-reading the file when it changed.
func (a *Accounts) lookup(name string) (User, bool) {
	if a.path != "" {
		if st, err := os.Stat(a.path); err == nil && !st.ModTime().Equal(a.mtime) {
			if users, err := LoadUsers(a.path); err == nil {
				a.users, a.mtime = users, st.ModTime()
				// Someone removed or demoted loses their session at once.
				for token, s := range a.sessions {
					if u, ok := users[s.user.Name]; (!ok || u.Role != s.user.Role) && (a.static == nil || a.static.Name != s.user.Name) {
						delete(a.sessions, token)
					}
				}
			}
		}
	}
	if u, ok := a.users[name]; ok {
		return u, true
	}
	if a.static != nil && a.static.Name == name {
		return *a.static, true
	}
	return User{}, false
}

// Verify checks a name and password. After too many wrong attempts for a
// name it refuses for a while, whatever the password; wait says how long.
func (a *Accounts) Verify(name, password string) (u User, ok bool, wait time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	name = strings.ToLower(strings.TrimSpace(name))
	f := a.fails[name]
	if f != nil && now.Before(f.until) {
		return User{}, false, f.until.Sub(now)
	}
	u, known := a.lookup(name)
	hash := a.decoy
	if known {
		hash = u.Hash
	}
	if CheckPassword(hash, password) && known {
		delete(a.fails, name)
		return u, true, 0
	}
	if f == nil || now.Sub(f.last) > lockDuration {
		f = &lockout{}
		if len(a.fails) > 10_000 { // names are attacker-chosen: do not let them pile up
			a.fails = map[string]*lockout{}
		}
		a.fails[name] = f
	}
	f.count, f.last = f.count+1, now
	if f.count >= maxFailures {
		f.until, f.count = now.Add(lockDuration), 0
		return User{}, false, lockDuration
	}
	return User{}, false, 0
}

// Open starts a session and returns its token.
func (a *Accounts) Open(u User) string {
	raw := make([]byte, 32)
	rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for t, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, t)
		}
	}
	a.sessions[token] = session{u, now.Add(sessionLife)}
	return token
}

// Session returns the account a session token belongs to.
func (a *Accounts) Session(token string) (User, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[token]
	if !ok || time.Now().After(s.expires) {
		delete(a.sessions, token)
		return User{}, false
	}
	// A changed users file may have removed or demoted this account.
	if u, known := a.lookup(s.user.Name); !known || u.Role != s.user.Role {
		delete(a.sessions, token)
		return User{}, false
	}
	return s.user, true
}

// ---- managing accounts from the console ----

// Account is what the console shows about an account.
type Account struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// Fixed accounts come from the environment (FLEETREEVE_DASHBOARD_AUTH) and
	// cannot be changed here.
	Fixed bool `json:"fixed,omitempty"`
}

// errNoFile is returned when the console has no accounts file to change.
var errNoFile = fmt.Errorf("this console was started without an accounts file (-users), so accounts cannot be changed here")

// reload reads the accounts file now. The caller holds the lock.
func (a *Accounts) reload() (map[string]User, error) {
	if a.path == "" {
		return nil, errNoFile
	}
	users, err := LoadUsers(a.path)
	if err != nil {
		return nil, err
	}
	return users, nil
}

// commit writes the accounts and makes them current. Sessions of the
// names given end: their password or role changed, or they are gone.
func (a *Accounts) commit(users map[string]User, ended ...string) error {
	if err := SaveUsers(a.path, users); err != nil {
		return fmt.Errorf("the accounts file could not be written: %v", err)
	}
	a.users = users
	if st, err := os.Stat(a.path); err == nil {
		a.mtime = st.ModTime()
	}
	for token, s := range a.sessions {
		for _, name := range ended {
			if s.user.Name == name {
				delete(a.sessions, token)
			}
		}
	}
	return nil
}

func admins(users map[string]User) int {
	n := 0
	for _, u := range users {
		if u.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// List returns every account, by name.
func (a *Accounts) List() []Account {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lookup("") // pick up changes made on disk
	var out []Account
	for _, u := range SortedUsers(a.users) {
		out = append(out, Account{Name: u.Name, Role: u.Role})
	}
	if a.static != nil {
		if _, shadowed := a.users[a.static.Name]; !shadowed {
			out = append(out, Account{Name: a.static.Name, Role: a.static.Role, Fixed: true})
		}
	}
	return out
}

// Add creates an account with a generated password, which it returns.
func (a *Accounts) Add(name, role string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	name = strings.ToLower(strings.TrimSpace(name))
	users, err := a.reload()
	switch {
	case err != nil:
		return "", err
	case !ValidUserName(name):
		return "", fmt.Errorf("that name cannot be used: lower-case letters, digits, dot, dash or underscore, starting with a letter or digit")
	case !ValidRole(role):
		return "", fmt.Errorf("the role must be viewer, operator or admin")
	}
	if _, exists := users[name]; exists || (a.static != nil && a.static.Name == name) {
		return "", fmt.Errorf("there already is an account called %s", name)
	}
	password := GeneratePassword()
	users[name] = User{Name: name, Role: role, Hash: HashPassword(password)}
	return password, a.commit(users)
}

// SetRole changes an account's role. The last admin cannot be demoted.
func (a *Accounts) SetRole(name, role string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	users, err := a.reload()
	if err != nil {
		return err
	}
	u, ok := users[name]
	switch {
	case !ok:
		return fmt.Errorf("there is no account called %s that can be changed here", name)
	case !ValidRole(role):
		return fmt.Errorf("the role must be viewer, operator or admin")
	case u.Role == RoleAdmin && role != RoleAdmin && admins(users) == 1:
		return fmt.Errorf("%s is the only admin; make someone else an admin first", name)
	}
	u.Role = role
	users[name] = u
	return a.commit(users, name)
}

// ResetPassword gives an account a new generated password and returns it.
func (a *Accounts) ResetPassword(name string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	users, err := a.reload()
	if err != nil {
		return "", err
	}
	u, ok := users[name]
	if !ok {
		return "", fmt.Errorf("there is no account called %s that can be changed here", name)
	}
	password := GeneratePassword()
	u.Hash = HashPassword(password)
	users[name] = u
	return password, a.commit(users, name)
}

// Remove deletes an account. The last admin cannot be removed.
func (a *Accounts) Remove(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	users, err := a.reload()
	if err != nil {
		return err
	}
	u, ok := users[name]
	switch {
	case !ok:
		return fmt.Errorf("there is no account called %s that can be removed here", name)
	case u.Role == RoleAdmin && admins(users) == 1:
		return fmt.Errorf("%s is the only admin; make someone else an admin first", name)
	}
	delete(users, name)
	return a.commit(users, name)
}

// ChangePassword lets someone replace their own password. keep is the
// session that stays signed in; their other sessions end.
func (a *Accounts) ChangePassword(name, current, next, keep string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	users, err := a.reload()
	if err != nil {
		return err
	}
	u, ok := users[name]
	switch {
	case !ok:
		return fmt.Errorf("this account is set in the console's environment; its password cannot be changed here")
	case !CheckPassword(u.Hash, current):
		return fmt.Errorf("the current password is not right")
	case len(next) < MinPassword:
		return fmt.Errorf("the new password needs at least %d characters", MinPassword)
	case next == current:
		return fmt.Errorf("the new password is the same as the current one")
	}
	u.Hash = HashPassword(next)
	users[name] = u
	kept, had := a.sessions[keep]
	if err := a.commit(users, name); err != nil {
		return err
	}
	if had {
		kept.user = u
		a.sessions[keep] = kept
	}
	return nil
}

// Close ends a session.
func (a *Accounts) Close(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}
