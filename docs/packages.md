# Packages

Every [release](https://github.com/nxtw/fleetreeve/releases) carries two
packages per architecture (x86_64/amd64 and aarch64/arm64), as RPM for
RHEL-family systems and as `.deb` for Debian and Ubuntu. `make packages`
builds the same from a checkout into `dist/` (`make rpm`, `make deb` for
one kind). It needs only Go, so it runs on any build machine, including
macOS; no `rpmbuild` or `dpkg` is involved.

| Package | Contains |
| --- | --- |
| `fleetreeve` | `/usr/bin/fleetreeve`, the `fleetreeve-pull` client service (installed but not enabled), `/etc/fleetreeve/pull.conf`, docs and examples |
| `fleetreeve-server` | the `fleetreeve-server` service, a `fleetreeve` system user, `/srv/fleetreeve` for your manifests, and the node builds the server hands out (`/usr/libexec/fleetreeve/agents`). Requires `fleetreeve`. |

```sh
dnf install ./fleetreeve-0.2.0-1.x86_64.rpm                      # controller or node
dnf install ./fleetreeve-server-0.2.0-1.x86_64.rpm               # the server; then: sudo fleetreeve setup

apt install ./fleetreeve_0.2.0-1_amd64.deb ./fleetreeve-server_0.2.0-1_amd64.deb     # Debian, Ubuntu
```

The version is that of the release: `make packages` takes it from the git
tag (`VERSION=0.2.0 RELEASE=1` on the command line overrides it).

**On a node**, the package replaces the curl bootstrap. After installing it,
join with the same command as before, minus the download:

```sh
fleetreeve install -server https://fleetreeve.example.com:8140 -join join.1795…
```

fleetreeve notices it was installed from a package and then only writes
`/etc/fleetreeve/token` and `/etc/fleetreeve/pull.conf` and enables the packaged
`fleetreeve-pull` service; `fleetreeve enroll` from a controller does the same. A
packaged client does not self-update - new versions arrive with
`dnf upgrade`, which restarts the service.

**Over SSH**, `fleetreeve enroll -rpm` installs the package and enrols the node
in one step, so nothing has to be done on the node by hand:

```sh
fleetreeve enroll -i inventory.yaml -l web -rpm dist/ \
    -server https://fleetreeve.example.com:8140 -secret-file server.secret -plan
```

`-rpm` takes a package file or a directory such as `dist/`, from which each
node gets the build for its architecture. The package is uploaded to
`/var/cache/fleetreeve/fleetreeve.rpm` and installed with `rpm -U`; running the same
command later with a newer (or rebuilt) package upgrades the node and
restarts its service. A node enrolled earlier without the package is
converted: its `/usr/local/bin/fleetreeve` and hand-installed unit are removed in
favour of the packaged ones.

**On the server**, put a checkout of your manifests in `/srv/fleetreeve`
(`site.yaml`, `inventory.yaml`), a certificate in `/etc/fleetreeve/tls/`
(`server.crt`, `server.key`, readable by group `fleetreeve`), and
`systemctl enable --now fleetreeve-server`. The secret is created in
`/var/lib/fleetreeve-server/secret`; reports go to `/var/lib/fleetreeve-server/reports`.
Mint tokens as that user, e.g.
`sudo -u fleetreeve fleetreeve token -secret-file /var/lib/fleetreeve-server/secret -ttl 24h -server https://fleetreeve.example.com:8140`.

The package definitions are in `packaging/` ([nfpm](https://nfpm.goreleaser.com)
format); set the `license` field there before distributing the packages.
They are unsigned: sign them (`rpm --addsign`) and publish them in a
repository (`createrepo_c`) if your hosts enforce GPG checks.
