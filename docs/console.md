# The web console

`fleetreeve dashboard` is a web console for a fleetreeve server: fleet compliance at a
glance, every host by role, what needs attention, and each host's last runs
down to the resource. It is **read-only** unless you enable rollback (see
below), and runs as its own process or container, next to the server rather
than inside it.

```sh
fleetreeve dashboard -demo                                   # look around with generated data
fleetreeve dashboard -server https://fleetreeve.example.com:8140 -secret-file server.secret
# then open http://localhost:8141
```

What it shows:

- The share of hosts in their desired state, and its trend since the
  dashboard started (the server keeps only the latest run of each host, so
  history begins when the dashboard does and is lost when it restarts).
- A matrix of every host grouped by role; each state has its own colour
  *and* shape, so it also reads in greyscale.
- Hosts that failed or went quiet, a searchable and sortable host table,
  and per host the runs it reported: what changed, what failed and why,
  and where each resource is declared.
- A log of every run and every request, newest first, that loads older
  entries as you scroll back; per host or for the fleet, and optionally
  only what changed or failed. The server keeps it (see below).
- Per host, everything applied on it: every resource it is kept to,
  grouped by the role or module that declares it (or by type), with a
  filter, and for each one whether fleetreeve created it and when fleetreeve last
  had to change it.
- Light and dark themes; it works down to phone width.

**Accounts.** Each person signs in with their own name and password:

```sh
fleetreeve user add ann -role operator -users /etc/fleetreeve/dashboard/users.yaml
# Added ann (operator).
# Password: kTm4x-Wq9Zp-…        (generated, shown once)
fleetreeve user add bob -users /etc/fleetreeve/dashboard/users.yaml      # role viewer
fleetreeve user role bob operator -users /etc/fleetreeve/dashboard/users.yaml   # change a role, keep the password
fleetreeve user list -users /etc/fleetreeve/dashboard/users.yaml
fleetreeve user rm bob -users /etc/fleetreeve/dashboard/users.yaml
```

Start the console with `-users <file>` (or `FLEETREEVE_DASHBOARD_USERS`). There
are three roles: a **viewer** may look; an **operator** may also roll back
(if the console was given the operator token); an **admin** may also manage
accounts in the console itself. `add` on an existing name sets a new
password and keeps the role; pipe a password in to choose your own
(`echo "$PW" | fleetreeve user add ann -users …`, at least 12 characters).

**In the console.** Once there is one admin (made with
`fleetreeve user add <name> -role admin`), the rest can be done from the
browser. Admins get an *Accounts* entry: add an account, change a role,
reset a password, remove an account. Everyone gets *Change password*.

- An admin never chooses someone's password: a new or reset account gets a
  generated one, shown once to the admin to pass on; its owner replaces it
  under *Change password*.
- Changing a role, resetting a password or removing an account signs that
  person out everywhere. Changing your own password signs out your other
  devices, not the one you did it on.
- You cannot remove your own account, and the last admin cannot be removed
  or demoted, so the console cannot be left without one.
- Every change is logged with who made it.
- The console must be able to write the accounts file: mount its directory
  read-write in the container.

- Passwords are stored as salted PBKDF2-SHA256 hashes; the file is
  readable by its owner only. A new file takes the owner of its directory,
  so hand the directory to the user the console runs as (uid 65532 in the
  container).
- The file is re-read when it changes: a new account works at once, and a
  removed or demoted one is signed out at its next request. No restart.
- Signing in gives a session of 12 hours, in a cookie that scripts cannot
  read and other sites cannot use. Sessions are kept in memory, so a
  restart of the console signs everyone out.
- Five wrong passwords for a name lock that name for five minutes.
- Sign-ins, failed attempts and sign-outs are logged with name and address;
  a rollback is recorded under the name of whoever asked.
- The console only accepts its own sign-in. It ignores a name and password
  sent with the request itself (HTTP basic auth): a browser keeps resending
  those and cannot be made to stop, which would defeat signing out.
  Scripts should use the [server API](api.md) with a token.
- `FLEETREEVE_DASHBOARD_AUTH=name:password` still works, as one extra operator
  account from the environment - useful to get started, but shared.
- Without any accounts the console is open to whoever can reach it, and
  read-only; it says so in its log.

**Run from the console.** Operators get a *Run* entry, and *Apply now* /
*Dry run* buttons on every host:

- **Apply the configuration now** on one host, a role, or all hosts,
  instead of waiting for the next scheduled run.
- **Dry run**: nothing is changed; the host reports what a run would do.
- **Run a command** (admins only, and only if enabled): any shell command,
  as root, on the chosen hosts; the panel shows each host's output and
  exit status as it comes in.

As with rollback, nothing connects to the hosts. A node running as a
service asks the server for requests between its scheduled runs - once a
minute by default (`fleetreeve pull -poll <duration>`, or `off`) - so a request
usually starts within a minute. Hosts that are down never pick theirs up;
it stays listed as waiting until withdrawn.

Apply and dry run need what rollback needs (the operator token and the
operator role) and give no new power: a host does what its next run would
do anyway. **Commands are different in kind - they are root on every
host** - so they are off unless you give the console a second token,
`FLEETREEVE_EXEC_TOKEN` (from `fleetreeve token -api exec`), and even then only
admins can use them. Every command is logged in full with who ran it, on
the console, on the server and on the host; its text and output are shown
to admins only. A command is stopped after its time limit (1 minute by
default, 10 at most) and its output is cut off at 64 KB.

**Rollback from the console.** A host's panel lists the earlier versions
the host has kept and the resources on hold, with *Roll back…* and
*Release…* next to them. After a confirmation the request is queued on the
server; the host collects it at its next check-in (within its pull
interval), carries it out, and reports the outcome, which the panel then
shows. A waiting request can be withdrawn. Nothing connects to the host:
this is the same pull as always, with a request riding along.

This is off unless you turn it on, and then it is deliberately narrow:

- Give the console the server's **operator token**
  (`FLEETREEVE_OPERATOR_TOKEN`, from `fleetreeve token -api operator`) *and* accounts.
  Then people with the operator role can roll back; viewers, and everyone
  on a console without accounts, cannot.
- The operator token can ask for exactly two things, a rollback and a
  release, and both are bounded by the versions the host itself kept. It
  cannot run commands, deliver content, register hosts or mint tokens.
- Who asked is recorded - the name of the signed-in person - in the
  server's log, on the host, and in the host's report.
- Requests are only accepted from the console's own pages, so another site
  cannot use a logged-in browser to make them.
- The server must be started with `-actions <dir>` (the container image
  and the RPM's service are).

How it is put together, and why:

- It reads the server's API with the **read token**
  (`$FLEETREEVE_API_TOKEN`, or derived from `-secret-file`), and by default has
  nothing else: it cannot change anything. No token ever reaches the browser.
- **Host variables are never shown**, since inventories tend to hold
  secrets; the dashboard strips them even if the server sends them.
- The whole UI is compiled into the binary: no CDN, no external fonts, no
  outbound calls from the browser. It works on an isolated network.
- `-tls-cert`/`-tls-key` serve HTTPS; use them, since people sign in here.
  For single sign-on, put the console behind your own proxy instead of
  giving it accounts.

**As a container:** `make image-dashboard` builds `fleetreeve-dashboard:<version>`
(about 10 MB) from `Containerfile.dashboard`. It is configured through the
environment - `FLEETREEVE_SERVER`, `FLEETREEVE_API_TOKEN`, `FLEETREEVE_CA`,
`FLEETREEVE_DASHBOARD_USERS`, and optionally `FLEETREEVE_OPERATOR_TOKEN` - and listens
on 8141.
[`packaging/container/`](../packaging/container) has a podman/systemd unit
(`fleetreeve-dashboard.container`) and a Compose service for it.
