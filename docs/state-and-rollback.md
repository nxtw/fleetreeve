# State and rollback

## State

Like Terraform, fleetreeve remembers what it manages, so the manifest is the
whole truth: **take a resource out of the manifest and the next apply
removes it from the node.** `plan` shows this before it happens.

```
$ fleetreeve plan site.yaml
  ~ file[/etc/app.conf]  content: sha256:92e7… → sha256:f787…  (changed outside fleetreeve)
  - file[/etc/app.d/old.conf]  no longer in the manifest, ensure: file → absent
  - file[/etc/hosts]  no longer in the manifest; left as it is and no longer tracked
```

- **Only what fleetreeve created is removed.** State records whether fleetreeve
  brought a resource into existence or found it already there. A package
  that was installed before fleetreeve managed it, or a file that already
  existed, is left as it is and simply no longer tracked. `file`, `package`,
  `user` and `group` can be undone; `exec`, `line` and `service` are only
  forgotten.
- Removal runs in reverse order of creation (files before their directory).
  A directory that still holds files fleetreeve does not own is not deleted; the
  run reports it as failed and keeps it in the state.
- **Drift is attributed.** If the manifest is unchanged since the last apply
  but the node differs, the change is flagged `(changed outside fleetreeve)`.
- **State lives on the node**, not on the controller: `/var/lib/fleetreeve/state/`
  as root, `~/.local/state/fleetreeve/` otherwise (`$FLEETREEVE_STATE_DIR` overrides).
  There is no backend to configure, and a laptop and a CI job share the same
  state. An apply takes a lock on the node; a second concurrent apply fails
  immediately rather than interleaving. `plan` needs no lock.
- State holds resource names, a digest of the desired attributes and
  timestamps — never attribute values, so no file contents or secrets.
- There is one state per manifest **name** and inventory host
  (`site@web1.json`). The name is the top-level `name:` key, or else the
  file name (`site.yaml` → `site`). Two different manifests applied
  to the same node need different names, or each would remove the other's
  resources.

```sh
fleetreeve state site.yaml                         # what is tracked on each host
fleetreeve state rm site.yaml 'file[/etc/motd]'    # stop tracking; leave it in place
fleetreeve apply -no-prune site.yaml               # this run: remove nothing
```

## Rollback

**Automatically, when a change breaks something.** Mark a resource with
`rollback: true`. fleetreeve keeps what was there before changing it; if anything
the resource notifies then fails in the same run - a service that will not
restart, a check that does not pass - the change is undone, and what it
notified is refreshed once more so it runs on the restored state:

```yaml
- file: /etc/app/app.conf
  template: templates/app.conf.tmpl
  rollback: true
  notify: service[app]

- service: app
  ensure: running
  notify: exec[app answers]          # optional: a health check after the restart

- exec: app answers
  command: curl -fsS http://127.0.0.1:8080/healthz
  refreshonly: true
```

```
  ✔ file[/etc/app/app.conf]  content: sha256:9a9f… → sha256:3462…
  ✘ service[app]  failed: refresh: restart: exit status 1 …
  ↩ file[/etc/app/app.conf]  rolled back: undone because service[app] failed; service[app] refreshed on the restored state
```

The run fails (exit 1) and reports the resource as rolled back, not as
changed; its state is left as before, so the next run tries again - until
the manifest or the data is fixed, the node stays on its last working
configuration. Resources that depend on the rolled-back one are skipped.
A failure anywhere down the notify chain counts, which is what makes a
health check useful. It works for `file`, `line`, `package` and `service`
(a package is installed or removed again, not downgraded); asking for it on
another type is a compile error. `validate:` remains the first line of
defence - it stops a bad file before it lands; rollback is for what only
shows once the service runs.

**By hand, when you decide.** Whenever fleetreeve changes a file, line, package
or service - marked or not - it first keeps a copy of how it was (the last
five per resource). `fleetreeve rollback` restores from those:

```sh
fleetreeve rollback site.yaml                                  # what can be restored, per host
fleetreeve rollback site.yaml 'file[/etc/app/app.conf]' -plan  # what would be restored
fleetreeve rollback site.yaml 'file[/etc/app/app.conf]'        # do it; again goes one version further back
fleetreeve rollback site.yaml 'file[/etc/app/app.conf]' -release
```

Restoring also refreshes what the resource notified (the service is
restarted on the old file), and puts the resource **on hold**: later runs
leave it alone and say so, because the manifest still asks for the state
you just rolled back and would otherwise bring it straight back - in pull
mode within the half hour. Fix the manifest or data, then `-release`. With
`-i`/`-l` it works on any set of hosts over SSH. In pull mode the same can
be asked for from the [dashboard](console.md) or the
[server API](api.md), without SSH: the host does it at its next check-in.

Things to know:

- Kept versions live on the node, next to its state
  (`/var/lib/fleetreeve/state/versions/`), readable by root only: a kept file is
  a full copy of its old content. Files over 32 MB are not kept; with
  `rollback: true` such a file is then not changed at all.
- A rollback restores what fleetreeve changed, nothing else: not the data a
  service wrote in the meantime, not a database migration an `exec` ran.
- Rolling back the *desired* state is a different thing, and usually the
  better one: revert the change in your manifests or data (a `git revert`),
  and the next run converges every host on it.
