# How it works

## How a remote run works

1. `ssh host uname -sm`, then upload the matching fleetreeve binary to
   `~/.cache/fleetreeve/agent-<hash>` if that exact build is not already there.
2. The binary reports the node's **facts**.
3. The controller **compiles** the host's catalog: variables, conditions,
   templates and source files are all resolved locally. Nodes never see your
   manifests, other hosts' data, or template logic — only their own catalog.
4. The catalog is piped to the binary over SSH; it locks the node's state,
   checks and applies each resource in graph order, removes what left the
   manifest, records the new state and streams results back as JSON lines.

Hosts run in parallel (`-forks`); the SSH connection is multiplexed, so a run
costs a few round trips per host regardless of catalog size.

## Layout

```
cmd/fleetreeve            entry point
internal/core        resource model, schemas, catalog, dependency graph, diff
internal/types       built-in resource types
internal/compile     manifests, inventory, templating -> catalog
internal/engine      check/apply/refresh/prune loop on the node
internal/state       per-node state file and lock
internal/remote      local and SSH executors, agent bootstrap
internal/pull        pull mode: catalog server, node client, reports, API
internal/dashboard   web console (UI compiled in)
internal/facts       fact discovery
internal/cli         commands and output
api/                 the server API as an OpenAPI document
packaging/           package definitions (RPM, deb), systemd units, container definitions
examples/            a runnable manifest per feature, a complete fleet, a pull-mode demo
docs/                this documentation
```

Most new resource types belong in a [module](modules.md) and need no change
to fleetreeve. A built-in one is one file in `internal/types`: implement `Schema`,
`Check` and `Apply` (optionally `Refresh`, `Validate`, `AutoRequire`,
`Absent`) and
call `core.Register`.

## Current limits

- Nodes: Linux and macOS, amd64/arm64. `user`, `group` and `service` are
  Linux-only; there is no launchd service provider yet.
- `package` manages presence/absence, not specific versions or `latest`.
- `become` uses passwordless `sudo -n`; there is no password prompt.
- No secrets backend: keep secrets out of the repo and pass them with `-e` or
  `{{ env "NAME" }}`; mark files `sensitive: true` to keep them out of diffs.
