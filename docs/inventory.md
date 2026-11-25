# Inventory

```yaml
vars: {admin_email: ops@example.com}
defaults: {user: deploy, become: true}        # connection settings for all hosts
groups:
  web: {hosts: [web1, web2], vars: {http_port: 8080}}
hosts:
  web1: {address: 10.0.0.11}
  web2: {address: 10.0.0.12, port: 2222, vars: {http_port: 9090}}
```

Host settings: `address`, `user`, `port`, `identity`, `become` (sudo),
`connection` (`ssh` or `local`), `ssh_args`, `host_key_check`, `vars`. Your
`~/.ssh/config`, agent and jump hosts apply, because fleetreeve uses the system
`ssh`. Without `-i`, fleetreeve manages the machine it runs on.

**Host keys.** SSH refuses a host whose key it does not know, or whose key
changed - which is every freshly built or rebuilt machine. To connect
anyway, without verifying the key:

```sh
fleetreeve enroll web3 -no-host-key-check          # this once
```

```yaml
defaults:
  host_key_check: false                       # always, for every host
hosts:
  scratch1: {host_key_check: false}           # or for one host
```

`no-host-key-check: true` in `.fleetreeve.yaml` does the same for every command
or, in a section, for one (`enroll: {no-host-key-check: true}`). Not
verifying means: whatever key the host presents now is taken for its key
and remembered, replacing any other it was known by. So with the option on
`enroll` only, a host is trusted when it is enrolled and recognised by
that key from then on - `plan` and `apply` still refuse an impostor.
With the option everywhere, nothing is ever refused. Either way the moment
of not verifying gives up what host keys are for: whoever answers for
that address then - a wrong machine, or someone in between - receives
your configuration and, with it, any secrets in it. Use it on networks you
trust, for machines that come and go; the node's own link to the server
stays verified regardless.
