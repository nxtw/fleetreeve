# Pull mode

## Pull mode (optional)

Push over SSH is the default. Pull mode is for nodes the controller cannot
reach (behind NAT, autoscaled, laptops), for fleets too large to fan out to,
or when nodes should keep enforcing their configuration without a controller
being up. Both modes use the same manifests, inventory, engine and state,
and can be mixed in one fleet.

```
node                                   server (fleetreeve serve)
 |-- facts ----------------------------> |  compile this host's catalog from
 | <------------------------- catalog -- |  the manifest + inventory
 |  apply (state, prune, refresh)        |
 |-- report ---------------------------> |  reports/<host>.json
```

As with push, the catalog is compiled centrally: nodes never see the
manifests, templates or other hosts' data. The server never connects to a
node and keeps nothing but the latest report of each host.

**Server** — next to a checkout of your manifests:

```sh
fleetreeve serve -i inventory.yaml -secret-file /etc/fleetreeve/server.secret \
    -tls-cert server.crt -tls-key server.key -reports /var/lib/fleetreeve/reports site.yaml
```

Manifest and inventory edits (a `git pull`) take effect on the next request;
no restart. Without `-tls-cert`/`-tls-key` it serves plain HTTP, which is
only appropriate behind a TLS-terminating proxy.

**Enrol nodes** — for hosts you can reach over SSH (once is enough), from
the machine that holds the server secret:

```sh
fleetreeve enroll -i inventory.yaml -l web -server https://fleetreeve.example.com:8140 \
    -secret-file /etc/fleetreeve/server.secret -plan      # preview
fleetreeve enroll -i inventory.yaml -l web -server https://fleetreeve.example.com:8140 \
    -secret-file /etc/fleetreeve/server.secret
```

Per host this installs `/usr/local/bin/fleetreeve` (the build matching the node's
platform), the node's token in `/etc/fleetreeve/token` (root-only), and a systemd
service `fleetreeve-pull` that pulls every 30 minutes (`-every` to change, `-ca`
to install a private CA), then enables and starts it. Enrolment is itself a
fleetreeve run of a built-in catalog: it is idempotent, re-running it upgrades the
binary or replaces the token and restarts the service, and
`fleetreeve enroll -i inventory.yaml -l web1 -remove` stops the service and
removes those files. It needs systemd and root on the node (`become: true`).

**Let nodes install themselves** — for nodes the controller cannot reach,
and for provisioning (kickstart, cloud-init, images). The server hands out
the fleetreeve build for the node's platform and an install script, so a node
needs nothing but `curl`. Mint a join token and run one command on the node
as root:

```sh
fleetreeve token -secret-file /etc/fleetreeve/server.secret -ttl 24h -server https://fleetreeve.example.com:8140
# Valid for 24h0m0s. Run on each new node (as root):
curl -fsSL https://fleetreeve.example.com:8140/install.sh | sh -s -- -server https://fleetreeve.example.com:8140 -join join.1795…
```

The script downloads the build, verifies its checksum and runs
`fleetreeve install`, which trades the join token for the node's own token and
sets up the same binary, token file and `fleetreeve-pull` service as
`fleetreeve enroll`. A join token works for any host **in the inventory** until
it expires, and for nothing else; the node's inventory name is its short
hostname unless you pass `-host`. `fleetreeve install -remove` undoes it.
[`examples/pull/README.md`](../examples/pull/README.md) has kickstart and
cloud-init snippets.

**Clients keep themselves current.** The installed service runs
`fleetreeve pull -self-update`: before each run it compares its own binary with
the server's build for its platform and, if they differ, replaces itself
(checksum-verified) and restarts in place. Upgrade the server and its
`agents/` directory, and the fleet follows on its next check-in.

`enroll` also **registers the host with the server**, so the server's
inventory does not have to be edited by hand for every new node. The host
is sent with its groups and its own `vars` from the controller's inventory
(not its address or login - the server never connects to nodes). The server
keeps registered hosts in its own directory (`fleetreeve serve -hosts <dir>`;
the container image and the RPM's service set it), next to - not inside -
the inventory file, which is usually a read-only checkout:

- A host already in the server's inventory file is left to that file.
- Group variables still come from the server's inventory. If the host is in
  a group the server does not define, `enroll` says so: the host is then a
  member of an empty group there.
- `fleetreeve enroll -remove` withdraws the registration, which revokes the
  node's access. `-no-register` skips registration altogether.
- `fleetreeve status -hosts <dir>` counts registered hosts as expected nodes.

`enroll` and `install` first check, from the node, that it can reach the
server at the address and with the CA it is about to be given, and change
nothing if it cannot - a mistyped address would otherwise leave the node
quietly enforcing its cached catalog and never reporting. `-skip-check`
turns the check off, for when the server is not running yet.

**Enrol by hand** — for nodes without systemd. A node is authorised when it
is listed in the inventory and presents the token for its own name:

```sh
fleetreeve token -secret-file /etc/fleetreeve/server.secret web1     # on the server
```

Put that token in a root-only file on the node and run fleetreeve there under
any service manager:

```sh
fleetreeve pull -server https://fleetreeve.example.com:8140 -token-file /etc/fleetreeve/token          # once
fleetreeve pull -server https://fleetreeve.example.com:8140 -token-file /etc/fleetreeve/token -every 30m
```

On an enrolled node, `sudo fleetreeve pull -plan` previews what its next
check-in would change, and `sudo fleetreeve pull` applies now; with no server
given, they use the node's own settings from `/etc/fleetreeve/pull.conf` for a
single run. This is the way to check a change from the server or a node:
`fleetreeve plan` and `fleetreeve apply` push over SSH from wherever they are run,
so they need SSH access to the hosts, which a pull-mode server does not have.

`-host` sets the inventory name (default: the short hostname), `-ca` trusts
a private CA, `-plan` only reports drift. With `-every`, runs are spread by
up to 10% so a fleet does not check in at once.

**See the fleet** — on the server:

```
$ fleetreeve status -reports /var/lib/fleetreeve/reports -i inventory.yaml
web1  site (pull)   changed  41s ago   13 resources · 2 changed · 1 refreshed
web1  fleet (push)  ok       2h ago    35 resources · in sync
web2  site (pull)   ok       3m ago    13 resources · in sync
db1   unknown  never reported
```

**Push runs are reported too**, so one place shows both. A node that is
enrolled with a server reports every run on it to that server by itself:
`fleetreeve apply` and `fleetreeve plan` from a controller over SSH, and by hand on
the node. Nothing is needed on the controller - no server address, no
secret: the node uses its own settings and token (`/etc/fleetreeve/pull.conf`),
as its pull service does. The controller says so when it is done:

```
$ fleetreeve apply -i inventory.yaml site.yaml
…
Reported by 2 host(s) to https://fleetreeve.example.com:8140
```

A node that is not enrolled reports nothing; one that cannot reach its
server, or was run without root, gets a warning, and the run's outcome
stays what it was. `-no-report` (or `no-report: true` in `.fleetreeve.yaml`)
keeps a run to yourself.

For hosts that are not enrolled, or to also report hosts the controller
could not reach, the controller can send the reports instead:

```sh
fleetreeve apply -i inventory.yaml site.yaml \
    -report-to https://fleetreeve.example.com:8140 -secret-file server.secret
```

The controller then sends one report per host after the run (also for hosts it
could not reach, with the error), authenticating with the server secret as
`fleetreeve enroll` does. The server keeps the latest report per host **and
manifest**, so a pushed `site` run and the node's own pull runs sit side by
side in `fleetreeve status`, marked `(push)` and `(pull)`. Hosts must be in the
server's inventory. A failure to report is printed as a warning and does
not change the run's exit status. As with pull reports, content diffs are
not sent.

It exits 1 if any node's last run failed or a node has gone **stale** - no
report for more than twice its pull interval - so it can back a monitoring
check. A stale node may be down, or unable to reach the server.

Things to know:

- **Server down:** a node falls back to the last catalog it received and
  keeps enforcing it. The catalog cache lives in the state directory, mode
  0600; like any catalog it contains the file contents destined for that
  node.
- **Join tokens** are for provisioning, so keep their lifetime short. Anyone
  holding a valid one can obtain the node token of any inventory host and
  with it that host's catalog; it grants nothing once expired and cannot be
  revoked earlier except by replacing the server secret.
- The server needs the node builds to hand out: `make agents` puts them in
  `bin/agents/` and `make install` next to the installed binary
  (`$FLEETREEVE_AGENT_DIR` overrides the location).
- **Revocation:** remove a host from the inventory (or, for a registered
  host, `fleetreeve enroll -remove` it) and its token stops working. Tokens are derived from the server secret (HMAC of the host
  name), so there is no per-node credential store; replacing the secret
  re-keys every node at once, and one node cannot be re-keyed alone.
- **Reports** carry resource names, statuses and property changes, but not
  content diffs — those stay on the node's terminal/journal.
- A broken manifest is refused at compile time on the server; nodes get an
  error and change nothing.
- The pull client refuses to send its token over plain `http://` to anything
  but localhost unless you pass `-insecure`.

## Certificates for hosts (optional)

By default a host proves who it is with a token the server derives from
its secret. Optionally a HashiCorp Vault PKI engine can be the certificate
authority instead:

```sh
fleetreeve serve … -vault-addr https://vault.example.com:8200 -vault-token-file /etc/fleetreeve/vault.token
```

Each host then gets a certificate signed by Vault at its next run - with a
key that never leaves it - shows it on every connection and renews it by
itself. Its token is good for that first certificate only; `-require-cert`
makes certificates mandatory; `fleetreeve enroll <host> -new-cert` starts a
rebuilt host over. Only the server talks to Vault.

```sh
fleetreeve vault setup     # prepare Vault: PKI engine, CA, role, policy, a token for the server
fleetreeve vault check     # can the server's token sign and revoke?
fleetreeve vault ca        # the CA certificate(s) hosts are verified against
fleetreeve vault certs     # every host's certificate and when it runs out
fleetreeve vault revoke web3
sudo fleetreeve vault deploy   # no Vault yet? run one on this machine, in a container
sudo fleetreeve vault webcert  # certificates from Vault for the web console and Vault's interface
```

`sudo fleetreeve setup` asks about Vault too, and can prepare it for you - or
run it for you, in a container on the server itself.

How it works, how to set Vault up, how to move an existing fleet over and
what the error messages mean: [Certificates for hosts](certificates.md).
