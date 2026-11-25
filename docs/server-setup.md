# Setting up a server

## Setting up a server

On the machine that is to be the server, with the `fleetreeve` package (or just
the binary) installed:

```sh
sudo fleetreeve setup
```

It asks a handful of questions, each with a sensible answer in brackets,
and then does the work:

| It asks | Default |
| --- | --- |
| how the server and the console run: as ordinary systemd services, or in podman containers | services |
| the name hosts reach the server by, and its port | this machine's full name, 8140 |
| where your configuration lives | `/srv/fleetreeve` |
| the certificate: make one, or use yours | make a self-signed one, valid 10 years |
| whether to install the web console, its port, and the first administrator's name and password | yes, 8141, a made-up password shown once |
| what the console may do: look, also run and roll back, also shell commands | run and roll back |
| the name of the server's own user for SSH | `fleetreeve` |
| whether HashiCorp Vault is the certificate authority for hosts; then whether it runs somewhere already or should be run here in a container (podman), its address, the PKI engine and role, and - for a Vault that exists - either a file with the server's Vault token or an administrator's token to prepare Vault with | no |
| whether this machine is itself the first host | yes |
| whether to open the ports in firewalld (when it runs) | yes |

When it is done you have: the server and the console as systemd services
(`fleetreeve-server`, `fleetreeve-dashboard`) running as an unprivileged user; a
certificate; [the server's own user and key](#the-servers-own-identity);
a console account; a small starting configuration with a `.fleetreeve.yaml`, so
that commands in that directory need no options; and this machine pulling
its configuration like any other host. It ends by telling you where to
sign in and how to add a host:

```sh
cd /srv/fleetreeve
fleetreeve enroll web1 -bootstrap-user root
```

`sudo fleetreeve setup -yes` takes every default without asking - for scripts
and images. Running setup again is safe: the secret, certificate, key,
accounts and your configuration are kept, the services are rewritten and
restarted, so it is also how you change a port or the console's powers.
**Services or containers.** The first question is how the server and the
console should run. As containers (`sudo fleetreeve setup -yes -container`
without questions; it takes podman) setup makes an image here from the
fleetreeve that is installed - the binary and the builds for hosts on an empty
base, nothing to download - and installs both as podman containers
managed by systemd (`/etc/containers/systemd/fleetreeve-server.container`,
`fleetreeve-dashboard.container`). Everything else is the same in either
form: the same user, files, directories, service names (`systemctl
status fleetreeve-server fleetreeve-dashboard`) and commands, because the
containers run as the server's own user of this machine and see the same
directories. The choice is made once: running setup again keeps the form
that is there. After upgrading fleetreeve, run setup again to make the image
anew from the new version.

On a machine that already runs the server in a [container](containers.md)
you set up yourself (the quadlet in `packaging/container`) it
installs nothing and leaves the server, its certificate, the console and
your configuration alone: it only asks about Vault as certificate
authority for hosts, and writes the answer into the container's
definition - see [Certificates for hosts](certificates.md#when-the-fleetreeve-server-runs-in-a-container).

## The server's own identity

Pushing and enrolling go over SSH, and SSH logs in as somebody. Left
alone, that is whoever types the command, with their personal key - so
your hosts end up trusting people's accounts, and the setup breaks when
someone leaves. Give the server an identity of its own instead:

```sh
sudo fleetreeve identity init
```

On the machine you run fleetreeve from, this creates a system user `fleetreeve`,
generates an SSH key pair for it (never replaced once it exists), and lets
administrators (the `wheel`, `sudo` or `admin` group) run `fleetreeve` - and
only `fleetreeve` - as that user. Then say so once:

```yaml
# .fleetreeve.yaml                     # inventory.yaml
run-as: fleetreeve                     defaults:
                                    user: fleetreeve
                                    become: true
```

From then on `plan`, `apply`, `enroll`, `state`, `facts`, `run`,
`rollback`, `lookup`, `status` and `token` hand themselves over to that
user through sudo, whoever types them: the hosts see one login, `fleetreeve`,
with one key, the server's. Personal accounts and keys play no part, and
the server's secret can be closed to everyone else (`identity init` does
that for the `secret-file` in `.fleetreeve.yaml`). Runs on the machine itself,
without an inventory, stay yours.

**First contact.** A new host does not know this user yet, so someone it
does know has to introduce it, once:

```sh
fleetreeve enroll web3 -bootstrap-user root        # or any login with passwordless sudo
```

This logs in as that account with *your* means (your key, agent or
password - it may ask), creates the `fleetreeve` user there, makes the
server's public key the only key that may log in as it, gives it sudo, and
notes the host's own key for the server. Then it enrols the host as usual,
already as `fleetreeve`. The bootstrap account is not used again and nothing of
yours is left on the host. A host that comes with the user and key built
in (`fleetreeve identity key` prints the public key for your image or
cloud-init) needs no `-bootstrap-user`.

Things to know:

- The `fleetreeve` user on a host has full sudo: it is how the server
  configures the host. Whoever can use the server's key is root on every
  host - as was already true of the server's secret. Guard both.
- `fleetreeve identity show` prints where the key is. Back the key up with the
  server's secret; a new key means bootstrapping every host again.
- The user is called `fleetreeve` unless `run-as` names another.
