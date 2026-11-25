# Examples

Build first (`make build`), then run from the repository root.

| Example | Shows | Needs |
| --- | --- | --- |
| [`demo.yaml`](demo.yaml) | A two-minute tour: files, a loop, a handler, a link | nothing |
| [`features/files.yaml`](features/files.yaml) | `file`: inline content, `source`, `template`, links, removal, `validate`, `sensitive` | nothing |
| [`features/lines.yaml`](features/lines.yaml) | `line`: edit single lines of a file you do not own | nothing |
| [`features/exec.yaml`](features/exec.yaml) | `exec` and its guards: `creates`, `unless`, `onlyif`, `refreshonly` | nothing |
| [`features/ordering.yaml`](features/ordering.yaml) | `require`, `before`, `notify`, `subscribe`, automatic ordering | nothing |
| [`features/loops-and-conditions.yaml`](features/loops-and-conditions.yaml) | `each`, `when`, list titles, every template function | nothing |
| [`features/failure.yaml`](features/failure.yaml) | A failing step: what is skipped, what still converges (exits 1 on purpose) | nothing |
| [`features/includes/`](features/includes/site.yaml) | Manifests as parameterised roles, included twice | nothing |
| [`features/variables/`](features/variables/site.yaml) | Inventory, groups, hosts and variable precedence, with three local "hosts" | nothing |
| [`fleet/`](fleet/site.yaml) | A complete project for CentOS/RHEL: web servers, load balancer, database, admins, SSH hardening, SELinux, firewalld | CentOS/RHEL-family servers |
| [`pull/`](pull/README.md) | Pull mode: server, tokens, enrolment, status; with a local demo script | nothing for the demo |

"Nothing" means it runs on this machine as your own user: those examples
only write under `/tmp/fleetreeve-examples` (`demo.yaml`: `/tmp/fleetreeve-demo`) and
keep their state in `~/.local/state/fleetreeve`. To put the state elsewhere, set
`FLEETREEVE_STATE_DIR`.

## A first session

```sh
fleetreeve plan  examples/features/files.yaml                 # what would happen; exit status 2
fleetreeve apply examples/features/files.yaml                 # do it
fleetreeve apply examples/features/files.yaml                 # "in sync"
echo oops >> /tmp/fleetreeve-examples/files/motd
fleetreeve plan  examples/features/files.yaml                 # drift, marked "changed outside fleetreeve"
fleetreeve apply examples/features/files.yaml -e listen=9090  # a one-line diff in app.ini
fleetreeve state examples/features/files.yaml                 # what fleetreeve tracks, created vs. merely tracked
```

## State: removing things by deleting them from the manifest

```sh
fleetreeve apply examples/features/loops-and-conditions.yaml
fleetreeve plan  examples/features/loops-and-conditions.yaml -e 'sites=["blog"]'
#   - file[.../sites/docs.conf]  no longer in the manifest, ensure: file -> absent
#   - file[.../sites/shop.conf]  no longer in the manifest, ensure: file -> absent
fleetreeve apply examples/features/loops-and-conditions.yaml -e 'sites=["blog"]'
```

`-e` stands in for editing the manifest here. The two files are removed
because fleetreeve created them; a file that existed before fleetreeve managed it
would be left alone and only dropped from the state.

## Several hosts without any servers

```sh
inv=examples/features/variables/inventory.yaml
fleetreeve validate -i $inv examples/features/variables/site.yaml
fleetreeve apply    -i $inv examples/features/variables/site.yaml
fleetreeve apply    -i $inv -l web examples/features/variables/site.yaml -e log_level=trace
cat /tmp/fleetreeve-examples/variables/*/resolved.txt
fleetreeve run -i $inv -- uname -sm                # ad-hoc command on every host
fleetreeve facts | head -40                        # the facts templates can use
```

## The fleet project

`fleet/` is laid out the way a real repository would be:

```
fleet/
  inventory.yaml        hosts, groups, how to reach them - and nothing else
  data/                 every value, by scope: common, os/, groups/, hosts/
  single-host.yaml      alternative inventory: every role on one VM
  site.yaml             which roles go on which hosts
  roles/                base, admins, ssh, selinux, web, loadbalancer, postgres (code only)
  modules/              fleetreeve.nginx and fleetreeve.firewalld: roles plus resource types;
                        fleetreeve.gitlab_runner: GitLab Runner, configured from data (not used by site.yaml)
  centos9.facts.json    facts of a CentOS 9 host, for checking offline
  templates/            nginx, haproxy, motd, backup script
  files/                files copied verbatim
```

It targets the Red Hat family (CentOS Stream, RHEL, Rocky, Alma) and needs
root on the hosts, so it cannot be applied on a laptop - but it can be
checked and inspected anywhere with the saved facts of a CentOS host
(`-facts centos9.facts.json`).

```sh
cd examples/fleet
fleetreeve validate -i inventory.yaml -facts centos9.facts.json site.yaml     # compiles every host's catalog
fleetreeve lookup   -i inventory.yaml -facts centos9.facts.json -l web2 site.yaml http_port open_ports
fleetreeve graph    -i inventory.yaml -facts centos9.facts.json -l web1 site.yaml | dot -Tsvg > web1.svg
```

Code and data are separate here. The roles hold no values and no
per-OS conditionals; `data/` holds every value, and `fleetreeve lookup` shows
which file a host's value comes from. Things to try: change `http_port` in
`data/hosts/web2.yaml` and look up `open_ports` for web2 and the
`haproxy.cfg` planned for lb1; add a key to `sysctl` in a group file and
see it merged with the common ones.

To try all of it on one throwaway CentOS/RHEL 9 VM, edit the address and
login in `single-host.yaml` and run it with that inventory; the VM becomes
web server, load balancer and database at once
(`curl http://<vm>/healthz` then goes through HAProxy to nginx). This is how
the example was tested, on CentOS Stream 9 with SELinux enforcing: one apply
converged it with no failures and a second reported "in sync".

```sh
fleetreeve plan  -i single-host.yaml site.yaml
fleetreeve apply -i single-host.yaml site.yaml
```

Point `inventory.yaml` at real machines and it becomes
`fleetreeve plan -i inventory.yaml site.yaml`. Things worth reading in it:

- `modules/`: two modules. `fleetreeve.nginx` adds a composite type, so a site
  is one resource (`fleetreeve.nginx.vhost`) that expands into its web root,
  start page and server block; `fleetreeve.firewalld` adds a script type, so an
  open port is a resource (`fleetreeve.firewalld.port`) with real state instead
  of a guarded command. `fleetreeve module list` and `fleetreeve types` show them.
- `roles/selinux.yaml` and the firewalld module: data-driven. Each group
  contributes its `open_ports`, `selinux_http_ports` and `selinux_booleans`
  in `data/groups/`, merged per host; `web2`, which overrides `http_port`
  in `data/hosts/web2.yaml`, gets a matching firewall rule and port label
  without being mentioned anywhere else. Both roles are
  skipped on non-Red Hat hosts (and the SELinux one where SELinux is
  disabled) via `.facts.os.family` and `.facts.selinux`.
- `roles/postgres.yaml`: `postgresql-setup --initdb` guarded by `creates`,
  a single `line` enabling a drop-in directory in the packaged
  `postgresql.conf`, a value computed from facts
  (`div .facts.memory.total_mb 4`) and a restart handler written as `exec`.
- `roles/loadbalancer.yaml` + `templates/haproxy.cfg.tmpl`: the backend
  list is generated from the inventory's `web` group, including the one
  host that listens on a different port.
- `roles/admins.yaml`: a chain of user -> home directory -> key file with
  no explicit ordering, and a sudoers file that is validated before it goes
  live.
- `roles/ssh.yaml`: a drop-in that sorts before the distribution's own, so
  it wins (sshd keeps the first value it reads), validated with `sshd -t`
  before it goes live. `ssh_allow_groups` in the inventory must include a
  group of the account fleetreeve logs in with, or you lock yourself out.
- `roles/base.yaml`: one file per sysctl setting, so removing a setting
  from the data removes its file from the hosts.

Mixed fleets: base, admins, ssh, web and loadbalancer also work on
Debian/Ubuntu. The few names that differ (`chronyd`/`chrony`, `sshd`/`ssh`,
the `nginx`/`www-data` user) are in `data/os/redhat.yaml` and
`data/os/debian.yaml`, not in the roles.

## Cleaning up

```sh
rm -rf /tmp/fleetreeve-examples /tmp/fleetreeve-demo ~/.local/state/fleetreeve/example-* ~/.local/state/fleetreeve/demo@* ~/.local/state/fleetreeve/pull-*
```
