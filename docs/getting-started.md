# Getting started

- [Install](#install)
- [A first manifest, on this machine](#a-first-manifest-on-this-machine)
- [A first remote host](#a-first-remote-host)
- [A first server](#a-first-server)
- [Where to go next](#where-to-go-next)

## Install

fleetreeve is one static binary. The machine you run it from (your laptop, a
pipeline, a server) needs that binary and an SSH client; the hosts you
manage need nothing but SSH and, for changes that take root, passwordless
`sudo`.

**A binary.** Each [release](https://github.com/nxtw/fleetreeve/releases) has
one per platform:

| Platform | File |
| --- | --- |
| Linux, x86_64 | `fleetreeve_linux_amd64.tar.gz` |
| Linux, arm64 | `fleetreeve_linux_arm64.tar.gz` |
| macOS, Apple silicon | `fleetreeve_darwin_arm64.tar.gz` |
| macOS, Intel | `fleetreeve_darwin_amd64.tar.gz` |

```sh
curl -fsSLO https://github.com/nxtw/fleetreeve/releases/latest/download/fleetreeve_linux_amd64.tar.gz
curl -fsSLO https://github.com/nxtw/fleetreeve/releases/latest/download/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
tar xzf fleetreeve_linux_amd64.tar.gz
sudo install fleetreeve /usr/local/bin/
fleetreeve version
```

The archive also holds `agents/`: the builds fleetreeve uploads to hosts of
other platforms than its own. Keep the directory next to the binary
(`/usr/local/bin/agents/`) if you manage hosts of another architecture or
operating system than the machine you run fleetreeve on.

**A package.** For RHEL-family systems and for Debian and Ubuntu, the
release has `fleetreeve` (the command, and the client service for pull mode)
and `fleetreeve-server` (adds the server service and the builds for hosts).
See [Packages](packages.md).

```sh
sudo dnf install ./fleetreeve-<version>-1.x86_64.rpm          # RHEL, CentOS Stream, Rocky, Alma, Fedora
sudo apt install ./fleetreeve_<version>-1_amd64.deb           # Debian, Ubuntu
```

**A container image**, for the server and the web console only:
`ghcr.io/nxtw/fleetreeve-server` and `ghcr.io/nxtw/fleetreeve-dashboard`. See
[Containers](containers.md).

**From source.** It needs Go (the version in `go.mod`) and nothing else:

```sh
git clone https://github.com/nxtw/fleetreeve && cd fleetreeve
make build agents     # bin/fleetreeve, and bin/agents/ for every platform
make test
```

## A first manifest, on this machine

Without an inventory, fleetreeve works on the machine it runs on. The demo
touches only `/tmp`, so it needs no root:

```sh
fleetreeve plan  examples/demo.yaml     # what would change, with diffs; changes nothing
fleetreeve apply examples/demo.yaml     # make it so
fleetreeve apply examples/demo.yaml     # again: everything is in sync, nothing happens
```

Change a file it made, and `plan` shows the difference and exits with
status 2: that is drift detection. `apply` puts it back.

A manifest is a list of resources - what should be true - not of steps:

```yaml
name: demo
resources:
  - file: /tmp/fleetreeve-demo
    ensure: directory
  - file: /tmp/fleetreeve-demo/hello.txt
    content: |
      Hello from fleetreeve.
    mode: 0644
```

`fleetreeve types` lists what can be managed (`file`, `package`, `service`,
`user`, `exec`, `line`, ...), and `fleetreeve types file` documents one.
[Manifests](manifests.md) explains ordering, variables, templates and
conditions.

## A first remote host

An inventory says which hosts exist and how to reach them:

```yaml
# inventory.yaml
defaults:
  user: deploy          # a login that may use sudo without a password
  become: true
hosts:
  web1: {address: 10.0.1.11}
```

```sh
fleetreeve plan  -i inventory.yaml site.yaml
fleetreeve apply -i inventory.yaml site.yaml
```

fleetreeve uses your SSH client, so whatever `ssh deploy@10.0.1.11` needs -
keys, an agent, a jump host in `~/.ssh/config` - applies here too. It
uploads its own binary to the host (a few megabytes, once per version),
sends that host's compiled catalog, and shows what was changed.
[`examples/fleet`](../examples/README.md) is a complete project to read
and copy from: web servers, a load balancer and a database, with roles,
data and modules. [Inventory](inventory.md) and [Data](data.md) describe
the pieces.

## A first server

Pushing from a laptop is all many setups need. A server adds hosts that
keep themselves in line - each fetches its configuration every half hour -
plus a history of every run and a web console. On a fresh machine with
fleetreeve installed:

```sh
sudo fleetreeve setup
```

It asks a handful of questions (every one has a default; `-yes` takes them
all) and leaves you with a running server and console, a starting
configuration in `/srv/fleetreeve`, and the machine itself as its first host.
Then, from `/srv/fleetreeve`:

```sh
fleetreeve enroll web1 -bootstrap-user <a login that exists on web1>
fleetreeve status
```

[Setting up a server](server-setup.md) describes the questions - services
or containers, the console, Vault - and [Pull mode](pull-mode.md) how
hosts and server work together.

## Where to go next

- [Manifests](manifests.md), [Data](data.md) and [Modules](modules.md), to
  describe your own hosts.
- [State and rollback](state-and-rollback.md), before the first change you
  want to undo.
- [Commands, settings and logging](commands.md), for the rest of the
  command line.
- The [documentation index](README.md).
