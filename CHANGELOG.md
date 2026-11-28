# Changelog

What changed for users, newest first. Versions follow
[semantic versioning](https://semver.org); while the major version is 0, a
minor version may change behaviour, and this file says where.

## 0.1.4

- **Conditions can be written the way they are read.**
  `when: docker_repo and facts.os.family == "redhat"` instead of
  `when: and .docker_repo (eq .facts.os.family "redhat")`, and
  `when: host.groups has "web"` instead of
  `when: contains "web" .host.groups`. There are `==`, `!=`, `<`, `<=`,
  `>`, `>=`, `has`, `in`, `not in`, `not`, `and`, `or`, brackets, and
  functions written `f(a, b)`; see
  [Manifests](docs/manifests.md). Conditions written the old way keep
  working and mean the same, so nothing has to be changed. Mistakes such
  as `=` for `==` or `&&` for `and` are explained, and so is comparing a
  number with a text or naming a variable that does not exist. The new
  section [Conditions](docs/manifests.md#conditions) has everything,
  with the older form next to the new one.

## 0.1.3

- **Hosts that have fleetreeve as a package now follow their server's
  version by themselves**, as hosts with the server's own build already
  did. A server keeps the client packages of its version
  (`/usr/share/fleetreeve/packages`, filled by `fleetreeve upgrade`) and
  offers them; at its next run a host with an older package fetches the
  new one, verifies its checksum and installs it, which restarts its
  service on the new version. Hosts never go back to an older version
  this way, and `FLEETREEVE_AUTO_UPDATE=no` in a host's
  `/etc/fleetreeve/pull.conf` keeps that host where it is. Hosts on 0.1.2
  or older need one last upgrade by hand or with
  `sudo fleetreeve upgrade -with-hosts`. A server upgraded from 0.1.2
  needs `sudo fleetreeve upgrade` run once more afterwards: only this
  version knows to keep the packages for hosts.

## 0.1.2

- **New: `sudo fleetreeve upgrade`** brings a machine to the newest
  release in one step: it fetches the release from GitHub, verifies the
  checksums, installs it the way fleetreeve was installed there (RPM,
  Debian package or the plain binary), and on a server makes the container
  image anew and restarts the server and the console on the new version.
  `-with-hosts` upgrades the hosts' packages too, `-from <dir>` works
  without access to the internet, `-plan` shows what would happen. See
  [Upgrading](docs/server-setup.md#upgrading). It takes its options from
  the command line only, never from `.fleetreeve.yaml`. Getting to this version
  from 0.1.1 is still done by hand, once: install the package and run
  `sudo fleetreeve setup -yes`.
- **New: `fleetreeve unenroll <host>`** takes a host out of pull mode. It
  is `fleetreeve enroll <host> -remove` under a name that can be found;
  that form keeps working.
- **Fixed: a host moved from one server to another could not reach its new
  server.** It kept the certificate of the old one, which the new server
  turned away before the host could ask for another ("remote error: tls:
  unknown certificate authority" in the host's log); it went on enforcing
  its last configuration and stopped reporting. `fleetreeve enroll` now
  notices such a certificate and replaces it, as `-new-cert` does. For a
  host that is stuck this way, enrol it again with this version.
- **The example module `fleetreeve.nginx` creates web roots owned by root**
  instead of by the nginx user (module version 0.2.0), so that a
  compromised nginx process cannot rewrite the content it serves. Projects
  that copied the module from `examples/` get the change when they copy it
  again; the owner of existing web roots then changes to root on the next
  run.

## 0.1.1

- **Fixed: a crash when catalogs for several hosts were compiled at the
  same moment** ("fatal error: concurrent map writes"). It could end a
  `plan` or `apply` over several hosts, and the server when hosts pulled
  together, in projects that use modules with script types. It was rare
  and a second run succeeded; nothing was left half-applied.
- `fleetreeve setup` run again keeps what the console may do (look only,
  apply and roll back, shell commands) as the default answer.
- The tests now also run under Go's race detector.

## 0.1.0

The first public release.

- **Resources and catalogs.** Manifests in YAML: `file`, `package`,
  `service`, `user`, `group`, `exec` and `line`; a dependency graph
  with `require`, `before`, `notify` and `subscribe`; variables, templates,
  conditions and loops; a catalog compiled and validated per host before
  anything is touched.
- **Agentless runs over SSH.** `plan` with real diffs and drift detection,
  `apply`, `validate`, `graph`, `lookup`, `facts`, ad-hoc `run`; hosts in
  parallel.
- **State, pruning and rollback.** What left the manifest is removed from
  the host; changes can be rolled back, per resource or per run.
- **Data layer.** Values per host, group and operating system, kept out of
  manifests.
- **Modules.** Roles and resource types - composite or script - in
  versioned, lockable modules.
- **Purging.** Directories that hold only what is managed.
- **Pull mode.** A server that compiles catalogs for hosts that fetch
  their own; enrolment with join tokens; reports, history, queued runs and
  rollbacks; self-updating clients.
- **Web console.** Fleet overview, per-host detail, a scrollable log,
  accounts with roles, apply, dry run and rollback from the browser.
- **Server API**, described in OpenAPI. Host variables and the text of queued
  shell commands are shown to the admin token only.
- **`fleetreeve setup`.** A complete server from a few questions, as systemd
  services or podman containers, with its own user and SSH key.
- **HashiCorp Vault as certificate authority.** Host certificates with
  automatic renewal; `fleetreeve vault` to prepare, check, deploy and unseal
  Vault; certificates from Vault for the web console and Vault's own
  interface.
- **Packages and images.** RPM and `.deb` packages, container images for
  the server and the console, static binaries for Linux and macOS.
