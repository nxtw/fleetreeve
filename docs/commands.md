# Commands, settings and logging

## Commands

| | |
| --- | --- |
| `fleetreeve plan <manifest>` | Dry run with diffs. Exit 0 in sync, 2 drift, 1 error. |
| `fleetreeve apply <manifest>` | Converge. `-every 30m` keeps enforcing. |
| `fleetreeve validate <manifest>` | Compile every host's catalog offline (with this machine's facts, or `-facts <file>`). |
| `fleetreeve lookup <manifest> [variable…]` | Each host's variables and where they come from. |
| `fleetreeve graph <manifest>` | Dependency graph as DOT (`\| dot -Tsvg`). |
| `fleetreeve rollback <manifest> [resource…]` | Restore resources to how they were before fleetreeve last changed them; `-release` ends the hold. |
| `fleetreeve state <manifest>` | What fleetreeve tracks on each host. `state rm <manifest> <resource>…` stops tracking. |
| `fleetreeve facts` | Facts as JSON. Custom facts: `/etc/fleetreeve/facts.d/*.json` on the node. |
| `fleetreeve run -- <command>` | Ad-hoc shell command on each host. |
| `fleetreeve types [type]` | Built-in documentation, including module types. |
| `fleetreeve module add\|install\|verify\|list\|init` | Extend fleetreeve with modules: roles and resource types. |
| `sudo fleetreeve upgrade` | Bring fleetreeve on this machine to the newest release, and a server's services with it. See [Upgrading](server-setup.md#upgrading). |

Common options: `-i inventory`, `-l hosts,groups`, `-e key=value`,
`-forks n`, `-report file.json`, `-no-prune`, `-v`, `-no-diff`.

## Settings file

Options that never change in a project can be written down once. fleetreeve
reads `.fleetreeve.yaml` from the current directory or the nearest parent that
has one, else `~/.config/fleetreeve/config.yaml`:

```yaml
server: https://fleetreeve.example.com:8140      # top level: every command
secret-file: server.secret
ca: server.crt

enroll:                                     # a command's name: only that command
  inventory: inventory.yaml
  rpm: dist
```

With that, enrolling a node - package, token, CA, service - is:

```sh
fleetreeve enroll web1 -plan
fleetreeve enroll web1 web2
```

and `fleetreeve token -ttl 24h` prints the ready-made join command, with no
flags to remember.

Keys are the long option names (`inventory` and `limit` stand for `-i` and
`-l`); relative paths are taken from the file's directory. Options on the
command line always win, a misspelt key is an error, and `enroll` prints
which file and server it is using. `$FLEETREEVE_CONFIG` names another file, or
`none` to ignore settings files.

Put settings at the top level only if every command should get them. An
`inventory` there, for instance, would make a bare `fleetreeve apply site.yaml`
target that inventory instead of this machine; keep it in a section.
Adding `report-to` at the top level makes every `plan` and `apply` report
to the server.

## Logging

Every command takes `-log-format text|json` (or `$FLEETREEVE_LOG_FORMAT`).

- `fleetreeve serve` logs **JSON by default**, as do the pull services set up by
  `fleetreeve enroll`, `fleetreeve install` and the RPM - so the journal of both the
  server and the nodes is structured.
- Commands you run by hand keep their readable output unless you ask for
  JSON: `fleetreeve apply -log-format json site.yaml`.

Each line is one object with `time`, `level` and `msg`, plus fields:

```json
{"time":"…","level":"INFO","msg":"resource","host":"web1","resource":"file[/etc/motd]","status":"changed","changes":[{"property":"content","from":"sha256:92e7…","to":"sha256:f787…"}],"drifted":true,"origin":"roles/base.yaml:24"}
{"time":"…","level":"ERROR","msg":"resource","host":"web1","resource":"exec[download]","status":"failed","error":"command: exit status 7: connection refused"}
{"time":"…","level":"INFO","msg":"run finished","host":"web1","mode":"apply","resources":34,"ok":32,"changed":1,"pending":0,"refreshed":0,"failed":1,"skipped":0,"forgotten":0,"duration_ms":412}
{"time":"…","level":"INFO","msg":"catalog served","remote":"10.0.1.11:40412","host":"web1","resources":34}
{"time":"…","level":"WARN","msg":"request refused","remote":"10.0.9.9:51022","path":"/v1/catalog","host":"web1","status":401,"reason":"invalid host or token"}
```

Run records (`resource`, `run finished`) go to stdout; everything else
(server requests, warnings, fatal errors) to stderr. `level` is `ERROR` for
failures, `WARN` for skipped resources and refused requests. Resources that
were already in the desired state are logged only with `-v`. Content diffs
are never logged, since logs travel further than a terminal does; property
changes show digests instead.

On a node: `journalctl -u fleetreeve-pull -o cat | jq 'select(.level=="ERROR")'`.

Not affected: output that is a command's result rather than a log
(`fleetreeve state`, `status`, `validate`, `graph`, `types`, `run`); `fleetreeve facts`
and `-report` files were JSON already.
