# Certificates for hosts: HashiCorp Vault as the authority

This is optional. Without it, fleetreeve works as described in the
[Pull mode](pull-mode.md): a host proves who it is with a
token. This page is about replacing that token with a certificate.

- [What it is](#what-it-is)
- [Switching it on](#switching-it-on)
- [What happens](#what-happens)
- [Requiring certificates](#requiring-certificates)
- [A host that was rebuilt, or lost its key](#a-host-that-was-rebuilt-or-lost-its-key)
- [Setting Vault up](#setting-vault-up)
- [No Vault yet: running one in a container](#no-vault-yet-running-one-in-a-container)
- [When the fleetreeve server runs in a container](#when-the-fleetreeve-server-runs-in-a-container)
- [Certificates for the web interfaces](#certificates-for-the-web-interfaces)
- [The `fleetreeve vault` commands](#the-fleetreeve-vault-commands)
- [Moving an existing fleet over](#moving-an-existing-fleet-over)
- [Where things are](#where-things-are)
- [When something is refused](#when-something-is-refused)
- [Things to know](#things-to-know)

## What it is

By default a host proves who it is with a token the server derives from
its secret. Optionally, a HashiCorp Vault PKI engine can be the
certificate authority instead: every host then has a certificate signed by
Vault, with a private key that never leaves the host, and shows that
certificate on every connection.

## Switching it on

```sh
fleetreeve serve … -tls-cert server.crt -tls-key server.key \
    -vault-addr https://vault.example.com:8200 -vault-token-file /etc/fleetreeve/vault.token
```

(`$VAULT_ADDR`, `$VAULT_TOKEN`, `$VAULT_CACERT` and `$VAULT_NAMESPACE` work
as they do for Vault's own tools. `-vault-pki` names the engine's mount,
default `pki`; `-vault-role` the role, default `fleetreeve`; `-vault-ca` a CA
file to verify Vault itself; `-cert-ttl 72h` asks for a lifetime other
than the role's.)

## What happens

- On its next run each host notices that the server works with
  certificates, makes a key, and sends a certificate request with its
  token. The server has Vault sign it - for exactly that host's name,
  whatever the request says - and the host stores the result next to its
  token (`/etc/fleetreeve/node.crt`, `node.key`). Nothing has to be done on the
  hosts, and nothing but the server ever talks to Vault.
- From then on the host shows the certificate. It renews it by itself when
  two thirds of its life are over, with a new key, showing the current
  certificate to get the next.
- **The token is good for the first certificate only.** Once a host has
  one, its token gets it no other, so a token that leaks later is of no
  use to anyone.
- The server remembers each host's current certificate (and, until the new
  one has been used, the one before). Any other certificate for that name
  is refused, however valid it is. So replacing a certificate takes effect
  at once and needs no revocation list to be distributed.
- Removing a host from the inventory or registry still cuts it off
  immediately, certificate or not.

## Requiring certificates

Without more, tokens keep working next to
certificates, which is what you want while hosts are picking theirs up.
Once `fleetreeve status` shows every host has run, add `-require-cert`: a token
alone then opens nothing but that first certificate request.

## A host that was rebuilt, or lost its key

Such a host has to start over, and only an administrator can let it:

```sh
fleetreeve enroll web3 -new-cert
```

That revokes the host's certificate in Vault, has the server forget it and
the host drop its old files; at its next run the host gets a new one with
its token. Taking a host out of pull mode (`enroll -remove`) revokes its
certificate too.

## A host that moves to another server

Enrol it with the new server, as you would a new host:

```sh
fleetreeve enroll web3
```

The host still holds the certificate its old server gave it, which the new
server's authority did not sign. `enroll` checks for that on the host and,
when the new server turns the certificate away, does what `-new-cert` does
and says so. (It checks as part of testing that the host can reach the
server, so not with `-skip-check`; give `-new-cert` yourself then.) The
same goes for hosts of a server whose authority was replaced. Afterwards,
take the host out of the old server's inventory.

If the server forgot a host's certificate while the host still has its
old files - the host was enrolled again from elsewhere, say - the host
notices at its next run that the server no longer knows its certificate,
drops it, and asks for a new one by itself.

## Setting Vault up

The short way is to let fleetreeve do it, with a Vault token that may
administer Vault:

```sh
export VAULT_ADDR=https://vault.example.com:8200
VAULT_TOKEN=<an administrator's token> fleetreeve vault setup -vault-token-file /etc/fleetreeve/vault.token
```

```
  ✔ mounted a PKI engine at pki/
  ✔ generated a CA in it ("fleetreeve hosts CA", valid 10 years; its key stays in Vault)
  ✔ role fleetreeve: client certificates for any name, valid 72h
  ✔ policy fleetreeve-pki-fleetreeve: sign with those roles, and revoke
  ✔ a token for the server (periodic: it lives as long as the server renews it, which it does hourly)
  ✔ checked: the server's token can sign and revoke
```

What is there already - the engine, its CA - is kept; the role and the
policy are (re)written; a new token is made each time. `sudo fleetreeve setup`
does the same when you answer its Vault questions with an administrator's
token, and also puts the options into the server's service.

Or by hand, which is also the description of what the above does. A PKI
engine, a role that may issue client
certificates for your host names, and a token for the server that may do
two things and nothing else:

```sh
vault secrets enable pki
vault secrets tune -max-lease-ttl=87600h pki
vault write pki/root/generate/internal common_name="fleetreeve hosts CA" ttl=87600h
    # or an intermediate of your existing CA

vault write pki/roles/fleetreeve allow_any_name=true enforce_hostnames=false \
    server_flag=false client_flag=true key_type=any ttl=72h max_ttl=72h

vault policy write fleetreeve - <<'POLICY'
path "pki/sign/fleetreeve"     { capabilities = ["update"] }
path "pki/sign/fleetreeve-web" { capabilities = ["update"] }   # only for the web interfaces' certificates
path "pki/revoke"         { capabilities = ["update"] }
POLICY

vault token create -policy=fleetreeve -period=720h -field=token > /etc/fleetreeve/vault.token
```

`key_type=any` matters: hosts make elliptic-curve keys, which a role
limited to RSA refuses. Restrict `allowed_domains` instead of
`allow_any_name` if your host names allow it. The token should be
*periodic* (`-period`): the server renews it every hour, so it lives as
long as the server runs; the server refuses to start with a token that
is about to expire and cannot be renewed.

**The period is how long the server may be off.** A periodic token that
nobody renews for its period is gone, and a server that then starts
finds Vault refusing its token and stops with a message saying so.
`fleetreeve vault setup` and `fleetreeve setup` make tokens with a period
of 30 days (`720h`), so that a machine switched off over a holiday comes
back by itself; versions before 0.1.5 used 24 hours. If it does happen,
with a Vault of your own: make a new token as above (or run `fleetreeve
vault setup` again, which keeps what is there and writes a new token) and
start the server. With Vault deployed by fleetreeve it is repaired
without you, see the next section.

## No Vault yet: running one in a container

If you have no Vault, fleetreeve can run one on the server machine itself, as
a podman container managed by systemd:

```sh
sudo fleetreeve vault deploy -vault-token-file /etc/fleetreeve/vault.token
```

```
  … fetching docker.io/hashicorp/vault:latest
  ✔ a certificate for fleetreeve.example.com (self-signed, valid 10 years): /etc/fleetreeve/vault/config/tls/vault.crt
  ✔ Vault runs in a container (docker.io/hashicorp/vault:latest), as service fleetreeve-vault, with its data in /var/lib/fleetreeve-vault
  ✔ initialised: the unseal key is in /etc/fleetreeve/vault/unseal.key and the root token in /etc/fleetreeve/vault/root.token (readable by root only)
  ✔ unsealed (the service does that by itself at every start)
  ✔ mounted a PKI engine at pki/
  …
  ✔ checked: the server's token can sign and revoke
```

With `-vault-token-file` it goes on to prepare Vault as `fleetreeve vault
setup` does, using the new root token; without it, it stops when Vault
runs. `sudo fleetreeve setup` does all of this when you answer "Nowhere yet:
run it on this machine" to its question where Vault is (or, without
questions: `sudo fleetreeve setup -yes -vault-deploy`), and starts the fleetreeve
server after Vault at boot.

Options: `-vault-addr https://<name>:<port>` for the address Vault is to
have (default: this machine's full name, port 8200; the name must resolve
on this machine), and `-vault-image <image>` for another image or a
pinned version, such as `docker.io/hashicorp/vault:1.20`. The default,
`:latest`, is fetched once and then stays what it is until you fetch
another yourself (`podman pull`, then `systemctl restart fleetreeve-vault`).

What it makes:

| | |
| --- | --- |
| `/etc/containers/systemd/fleetreeve-vault.container` | the service: `systemctl status fleetreeve-vault`, `journalctl -u fleetreeve-vault`. It starts at boot |
| `/etc/fleetreeve/vault/config/` | Vault's configuration (`vault.hcl`: one node with integrated storage, HTTPS only, web interface on) and its certificate (`tls/vault.crt`, which is also what `-vault-ca` and `$VAULT_CACERT` take) |
| `/var/lib/fleetreeve-vault/` | Vault's data, encrypted with a key that only the unseal key opens |
| `/etc/fleetreeve/vault/unseal.key` | the unseal key, readable by root only |
| `/etc/fleetreeve/vault/root.token` | the root token, readable by root only |

Running it again is safe: the certificate, the data and the keys are
kept; only the configuration file and the service definition are written
anew.

**Read this before you rely on it.**

- **The unseal key is kept on the machine** so that the service can
  unseal Vault by itself after a restart (`fleetreeve vault unseal`, which the
  service runs each time it has started). That is convenient and it is a
  trade: whoever can read root's files on this machine has the CA. If
  that is not good enough, move `unseal.key` away. Vault then starts
  sealed after every restart, and someone has to run `sudo fleetreeve vault
  unseal` and type the key in. Until then no certificates are issued,
  and a fleetreeve server that is started meanwhile waits (it reads the CA
  from Vault when it starts, and systemd keeps trying); one that is
  running keeps serving hosts whose certificate is still valid.
- **Keep a copy of the unseal key somewhere else.** There is exactly one,
  and without it the data in `/var/lib/fleetreeve-vault` cannot be read again:
  the CA would be gone and every host would need a new certificate from a
  new one.
- **The root token may do anything in Vault.** Use it to make yourself
  another way in (an administrator's policy and login of your own),
  store it somewhere safe, and delete the file. fleetreeve needs it for one
  thing only, replacing the server's token should that run out (below);
  the server itself has its own token, which may sign and revoke and
  nothing else. (Running `fleetreeve setup` again keeps that token as long as
  it works.)
- **After the machine was off for a long time, the server's token is
  replaced by itself.** The server keeps its token alive by renewing it;
  when the machine was off for longer than the token's period, Vault no
  longer takes it. Each time Vault starts, right after it is unsealed and
  before the fleetreeve server starts, the token is checked, and when it is
  gone a new one is made with the root token and put in the server's
  token file (`journalctl -u fleetreeve-vault` says when that happened).
  This needs the root token on the machine: if you took it away, as
  suggested above, the same place in the log gives the command to make a
  token with an administrator's token instead.
- **The port is not opened in the firewall.** Hosts never talk to Vault;
  only the fleetreeve server does, on the same machine. Open it
  (`firewall-cmd --add-port=8200/tcp --permanent`) only if you want to
  reach Vault or its web interface from elsewhere.
- **This is one Vault on one machine**: no standby, and no backups unless
  you make them (`vault operator raft snapshot save <file>`, with Vault's
  own tool and a token that may). It suits being the CA for fleetreeve's
  hosts; if Vault is to hold more than that, give it a home of its own.
- To use Vault's own tools against it: `export
  VAULT_ADDR=https://<name>:8200
  VAULT_CACERT=/etc/fleetreeve/vault/config/tls/vault.crt`.
- To take it away again: `systemctl stop fleetreeve-vault`, remove
  `/etc/containers/systemd/fleetreeve-vault.container`, `/etc/fleetreeve/vault` and
  `/var/lib/fleetreeve-vault`, and `systemctl daemon-reload`. The CA goes with
  it: take the Vault options off the fleetreeve server first (hosts then use
  their tokens again).

## When the fleetreeve server runs in a container

(This is about a server container you installed yourself. One that
`fleetreeve setup` installed - its container choice - is set up again in full
by running setup again, Vault included; the Vault options stand between
the same marker lines.)

Where the server runs as a podman container managed by systemd
(`/etc/containers/systemd/fleetreeve-server.container`), `sudo fleetreeve setup`
installs nothing. It asks the Vault questions only - is Vault to be the
certificate authority, does it run somewhere already or should it be run
here, and so on - and then:

- runs Vault in its own container, if you asked for that (as `fleetreeve vault
  deploy` does);
- prepares Vault and puts the server's token in
  `/etc/fleetreeve/server-vault/token`, owned by the user the server runs as
  in its container, with a copy of Vault's CA next to it (`ca.pem`);
- writes into the container's definition, between two marker lines, the
  mount of that directory and the server's command with the Vault
  options added;
- restarts the server and checks that it answers with certificates on.
  If it does not, the definition is put back as it was and the server is
  started again as before.

```
# >>> written by fleetreeve setup (Vault as certificate authority); run it again to change this
Volume=/etc/fleetreeve/server-vault:/etc/fleetreeve/server-vault:ro,Z
Exec=serve -vault-addr https://fleetreeve.example.com:8200 -vault-token-file /etc/fleetreeve/server-vault/token -vault-pki pki -vault-role fleetreeve -vault-ca /etc/fleetreeve/server-vault/ca.pem -i inventory.yaml …
# <<< written by fleetreeve setup
```

Everything else in the file is yours and is left alone. The command is
taken from an `Exec=` line of your own if the file has one, and otherwise
from the image. Running setup again changes the answers (requiring
certificates once every host has one, for instance); answering "no" to
Vault takes the marked lines out again.

Things to know:

- Run it from your configuration directory (where `.fleetreeve.yaml` names
  the server), so that setup can ask the server itself whether it came
  back with certificates on. Elsewhere it settles for the service
  staying up.
- The server's container must be able to reach Vault at the address you
  give. For a Vault deployed on the same machine that is the machine's
  own name, which a container would take to be itself; setup therefore
  adds `AddHost=<that name>:host-gateway` to the marked lines, so that
  the name leads to the host and Vault's published port there.
- For a Vault elsewhere, give the CA file that verifies it: the server's
  image holds no list of public certificate authorities.
- `fleetreeve vault check -vault-token-file /etc/fleetreeve/server-vault/token`
  (as root) checks the token the server uses.

## Certificates for the web interfaces

With Vault as certificate authority, the web interfaces on the server
machine get their certificate from it too, instead of one they signed
themselves:

- **the fleetreeve console** (port 8141), and
- **Vault's own interface** (port 8200), where fleetreeve runs Vault
  (`fleetreeve vault deploy`).

`sudo fleetreeve setup` and `sudo fleetreeve vault deploy -vault-token-file …` do
this as part of switching Vault on. By hand, and afterwards:

```sh
sudo fleetreeve vault webcert -vault-addr https://fleetreeve.example.com:8200 \
    -vault-token-file /etc/fleetreeve/vault.token     # the server's token
```

```
  ✔ The console has a certificate from Vault, valid 90 days, for fleetreeve.example.com, fleetreeve, localhost (there is none yet)
  ✔ Vault's own interface has a certificate from its CA, valid 90 days (the present one is not from Vault)
  ✔ The fleetreeve server was restarted, to verify Vault by its CA from now on
  ✔ A daily check renews them in time (systemctl list-timers fleetreeve-webcert)
```

**What your browser makes of it.** A certificate from Vault's CA is
trusted by a browser that trusts that CA, and by no other. Import the CA
once - `fleetreeve vault ca > fleetreeve-ca.crt`, then add it to your browser's or
operating system's authorities - and both interfaces are trusted from
then on, through every renewal. Without that the browser warns as it did
before. (If the PKI engine holds an intermediate of a CA your
organisation already trusts, there is nothing to import.)

How it works:

- A second role in the PKI engine, `fleetreeve-web` (`<role>-web`), issues
  *server* certificates for exactly the names of this machine - nothing
  else - valid 90 days. The server's token may sign with it. `fleetreeve
  setup` makes the role when it prepares Vault; for a Vault you prepare
  yourself: `fleetreeve vault setup -web-names fleetreeve.example.com,fleetreeve`, with
  an administrator's token.
- The names are the server's name as given to setup and this machine's
  full and short name, plus `localhost`. `-web-names a,b` gives others;
  the role has to allow them.
- The console's certificate and key are `/etc/fleetreeve/tls/console.crt` and
  `console.key`; its service definition (unit file or container) is
  pointed at them and the console restarted. Vault's are where they were
  (`/etc/fleetreeve/vault/config/tls/`); Vault is told to read them again,
  without a restart.
- `fleetreeve-webcert.timer` runs the command daily. It does nothing until a
  third of a certificate's life is left, then replaces it and reloads
  the service concerned.
- **The fleetreeve server's own certificate (port 8140) is not touched**:
  hosts were enrolled with it and verify the server by it.
- When Vault's interface changes from its self-made certificate to one
  from its CA, whatever verified Vault by the old one has to follow: the
  fleetreeve server is restarted once for that (its `-vault-ca` file then
  holds the CA). For Vault's own tools, `$VAULT_CACERT` keeps working
  with the same file.
- If the machine was off so long that Vault's certificate expired, the
  command gives Vault a temporary self-made one first, and then gets a
  new one with it.
- Switching Vault off in `fleetreeve setup` removes the timer and puts the
  console back on the server's certificate.

## The `fleetreeve vault` commands

| | Talks to | Needs |
| --- | --- | --- |
| `sudo fleetreeve vault deploy` | this machine | root, podman and systemd |
| `fleetreeve vault unseal` | Vault | the unseal key: from `/etc/fleetreeve/vault/unseal.key` (so: root), or typed in |
| `sudo fleetreeve vault webcert` | Vault, and this machine | root, and the server's token (`-vault-token-file`) |
| `fleetreeve vault setup` | Vault | an administrator's token (`$VAULT_TOKEN`, or it asks), and `-vault-token-file <file>`: where to write the token it makes for the server |
| `fleetreeve vault check` | Vault | the server's token (`-vault-token-file`) |
| `fleetreeve vault ca` | Vault | nothing: the CA is public |
| `fleetreeve vault certs` | the fleetreeve server | the server's address and secret, as `fleetreeve status` |
| `fleetreeve vault revoke <host>…` | the fleetreeve server | the same |

All take `-vault-addr` (or `$VAULT_ADDR`), `-vault-pki` and `-vault-role`
where they talk to Vault, and `$VAULT_CACERT` / `-vault-ca` and
`$VAULT_NAMESPACE` as Vault's own tools do.

**`setup`** prepares Vault as shown above. Options: `-cert-ttl 72h` for
the lifetime of host certificates, and `-domains example.com,lab.example.com`
to let the role issue certificates only for names in those domains instead
of for any name. Running it again is safe, but note that it writes the
role anew, with the lifetime and domains of *that* run.

**`deploy`** and **`unseal`** are for a Vault that fleetreeve runs itself; see
[above](#no-vault-yet-running-one-in-a-container).

**`check`** answers "will this work?" without a fleetreeve server: Vault
answers, the CA is there and until when it is valid, the token is accepted
and will not run out, and - by actually doing it with a throwaway
certificate for a name nobody uses - the token can sign with the role and
revoke.

```
$ fleetreeve vault check -vault-token-file /etc/fleetreeve/vault.token
  ✔ Vault answers at https://vault.example.com:8200
  ✔ CA at pki/: fleetreeve hosts CA (valid until 18-11-2036)
  ✔ the token is accepted, and will not run out on the server
  ✔ it can sign with role fleetreeve, and revoke (tried with a throwaway certificate)
```

**`ca`** prints the CA certificate, or chain, as PEM - for whatever else
should trust your hosts' certificates.

**`certs`** lists what the server knows: every host that has a
certificate, until when it is valid, and its serial number as Vault knows
it; and whether certificates are required.

```
$ fleetreeve vault certs
web1  valid until 24-11-2026 17:14  24:5b:1e:6e:87:14:9a:67:46:c9:5e:d9:76:e0:e3:3a:17:39:cf:7b
web2  valid until 24-11-2026 17:20  4b:fb:56:a0:81:b7:9f:72:27:5f:be:02:6c:11:d3:9e:50:aa:17:c4
2 host(s) with a certificate. Hosts must show their certificate.
```

**`revoke`** revokes a host's certificate in Vault and has the server stop
accepting it, at once. The host stays known, so at its next run it is told
its certificate no longer counts, and gets a new one with its token. Use
it when a certificate or key may have been copied. It does not keep a host
out: for that, remove the host from the inventory, or `fleetreeve enroll
-remove` it. (For a host that was rebuilt, `fleetreeve enroll <host> -new-cert`
does the same and also clears the old files on the host.)

## Moving an existing fleet over

1. Set Vault up as above (`fleetreeve vault setup`, or `fleetreeve vault deploy`
   if there is none), and check it (`fleetreeve
   vault check`).
2. Start the server with `-vault-addr` and `-vault-token-file`, **without**
   `-require-cert`. Hosts keep working with their tokens.
3. Wait for every host to have run once. Each gets its certificate at that
   run: `fleetreeve vault certs` shows who has one.
4. Add `-require-cert` and restart the server. From now on a token alone
   gets a host nothing but a first certificate.

Going back is removing the options again: tokens were never taken away
from the hosts, so they simply count again.

## Where things are

| | |
| --- | --- |
| On a host | `/etc/fleetreeve/node.crt` (the certificate and its issuers) and `/etc/fleetreeve/node.key` (the private key, readable by root only), next to the token |
| On the server | one small file per host in `<data>/certs/` (next to the reports; `-certs <dir>` puts it elsewhere) with the serial number and expiry of the host's current certificate |
| In Vault | every certificate it signed, under the PKI engine, and its revocation list |
| In the server's log | `certificate issued` (with `shown: token` for a first one, `shown: certificate` for a renewal), `certificate refused`, `certificate forgotten` |
| Over the API | `GET /v1/health` says `"certificates": "vault"` and whether they are required; `POST /v1/cert` is what hosts call; `GET /v1/certs` and `DELETE /v1/certs/{host}` are what `fleetreeve vault certs` and `revoke` use |

## When something is refused

| A host is told | It means | Do |
| --- | --- | --- |
| *this server only accepts hosts that show their certificate* | certificates are required and the host showed none | nothing: `fleetreeve pull` gets it one, unless the next line applies |
| *this host already has a certificate, so its token no longer gets it one* | a certificate was asked for with the token although the host has a valid one | if the host was rebuilt or lost its key: `fleetreeve enroll <host> -new-cert`. If not, someone else has the host's token: look into that |
| the host's log says *remote error: tls: unknown certificate authority* | the host shows a certificate from another server, or from an authority this server no longer uses | `fleetreeve enroll <host>` from this server: it replaces the certificate |
| *this is not the current certificate of …* | a certificate that was since replaced is being shown | on the host itself this does not normally happen; elsewhere it is a copy of an old certificate being tried |
| *the server has no record of a certificate for …* | the host was started over on the server | nothing: the host drops the old certificate and asks again at the same run |
| *the certificate authority did not sign the request* | Vault refused or could not be reached | the message carries Vault's own words; most often the role (`key_type`, allowed names) or the token's policy |

The server itself can be refused too:

| The server's log says | It means | Do |
| --- | --- | --- |
| *the Vault token is not accepted* (and the service keeps restarting) | the server's token ran out: it was not renewed for its period, as when the machine was off for long. Or it was revoked | with Vault deployed by fleetreeve: `sudo systemctl restart fleetreeve-vault`, which replaces the token. Otherwise `fleetreeve vault setup` with an administrator's token, then start the server |

## Things to know

- **Short lifetimes are the point.** With `ttl=72h` a host that is
  switched off for three days comes back with an expired certificate; it
  then simply gets a new one with its token, since the old one no longer
  counts. Nothing to clean up.
- **Vault must be reachable when certificates are issued**, not otherwise:
  hosts that have a valid certificate keep working while Vault is down.
- **The server's own certificate** (`-tls-cert`) is separate. It can come
  from the same Vault - issue it there and give hosts Vault's CA as `-ca`
  when enrolling - but fleetreeve does not fetch or renew it for you. (It
  does for the web console and for Vault's interface: see
  [above](#certificates-for-the-web-interfaces).)
- The CA chain is read from Vault when the server starts; after rotating
  the CA in Vault, restart the server.
- The console and the API tokens are not affected: certificates are for
  hosts.
- Whoever holds the server's secret can still enrol hosts and start them
  over; whoever holds the Vault token can have certificates signed. Both
  are the server's, and both deserve the same care.
