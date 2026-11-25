# Contributing to fleetreeve

Thank you for considering it. This page says how to report a problem, how
to propose a change, and what a change needs before it can be merged.

## Reporting a bug

Open an [issue](https://github.com/nxtw/fleetreeve/issues) with:

- what you ran (the command line, and the smallest manifest that shows it),
- what you expected and what happened instead (the output; `-v` adds detail),
- `fleetreeve version`, and the operating system of the controller and the host.

Take passwords, tokens and host names you would rather not publish out of
what you paste. For anything that could be a security problem, do not open
an issue: see [SECURITY.md](SECURITY.md).

## Proposing a change

For anything larger than a fix, open an issue first and describe the
problem you want solved. That saves you from writing something that does
not fit. Many additions do not need to be in fleetreeve at all: a new resource
type or role belongs in a [module](docs/module-development.md), which
anyone can publish without changing fleetreeve.

## Working on the code

It needs Go, in the version `go.mod` names, and nothing else.

```sh
make build agents     # bin/fleetreeve and the builds for hosts
make test             # go vet and every test
bin/fleetreeve apply examples/demo.yaml
```

[How it works](docs/how-it-works.md#layout) maps the packages. In short:
`internal/core` is the resource model, `internal/types` the built-in
types, `internal/compile` turns manifests into catalogs, `internal/engine`
applies a catalog on a host, `internal/remote` gets it there,
`internal/pull` is the server, `internal/dashboard` the console, and
`internal/cli` the commands.

## What a pull request needs

- **Tests.** New behaviour comes with a test that fails without it;
  `make test` passes. Tests must not need root, a network or a real host:
  the existing ones show how to stand in for those.
- **Documentation.** A new option, command or behaviour is described where
  a user would look for it under `docs/`, and in the command's usage text.
  An API change is made in `api/openapi.yaml` too (a test compares it with
  the server).
- **No new dependencies** without a strong reason. fleetreeve has one, the YAML
  parser, and staying a single static binary is a feature.
- **Compatibility.** Manifests and state files that work today keep
  working. If something has to break, say so in the pull request and in
  `CHANGELOG.md`.
- **A changelog entry** under "Unreleased" for anything a user will notice.
- **Formatting.** `gofmt`; comments that say why, not what.

Keep a pull request to one subject. Commit messages: a short summary line
in the imperative ("Add purge to file directories"), then - where it is
not obvious - why.

## Licence of contributions

fleetreeve is licensed under the [Apache License 2.0](LICENSE). By submitting a
contribution you agree that it is licensed under the same terms (section 5
of the licence).

## Releasing (maintainers)

1. Move the "Unreleased" entries in `CHANGELOG.md` under a new version
   heading with the date.
2. Tag it: `git tag -a v0.2.0 -m "fleetreeve 0.2.0" && git push origin v0.2.0`.
3. The release workflow runs the tests, builds the binaries, packages and
   checksums, publishes them as a GitHub release, and pushes the container
   images. `make release` builds the same files locally into `dist/`.

Versions follow [semantic versioning](https://semver.org). While the major
version is 0, a minor version may change behaviour; the changelog says
where.
