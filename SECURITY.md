# Security policy

## Reporting a vulnerability

Please do not report security problems in public issues.

Use GitHub's private reporting instead: on the repository's
[Security tab](https://github.com/nxtw/fleetreeve/security/advisories/new),
choose "Report a vulnerability". Say what is affected, how to reproduce
it, and what an attacker could do with it. We aim to answer within a
week; if the report holds, a fix and a release follow, and you are
credited in the advisory unless you prefer not to be.

## Supported versions

Fixes are made on the latest release. While fleetreeve is at version 0.x there
are no maintenance branches for older versions.

## What fleetreeve handles, and where to look

fleetreeve changes systems as root and moves credentials around, so these are
the parts where a mistake matters most:

- **The server's secret** (`secret` in its data directory). Every host
  token, join token and API token is derived from it. Whoever has it can
  enrol hosts and use the API with any role.
- **Host tokens and certificates** (`/etc/fleetreeve` on each host). A token
  lets its holder fetch that one host's catalog, which may hold secrets
  meant for that host.
- **Catalogs.** A host only ever receives its own compiled catalog, never
  manifests, other hosts' data or templates.
- **The web console.** Password hashes are in its accounts file; what
  accounts may do is limited by role and by which API tokens the console
  was given.
- **Vault.** With Vault as certificate authority, the server's Vault token
  may sign for two roles and revoke, nothing else. A Vault deployed by
  `fleetreeve vault deploy` keeps its unseal key and root token on the machine,
  readable by root only; [the documentation](docs/certificates.md) says
  what that means.
- **Modules.** A module's script types run as root on hosts. Installing a
  module is trusting its author; `modules.lock` pins what was reviewed.

The documentation describes the intended behaviour of each. Anything that
lets someone go beyond it is a vulnerability we want to hear about.
