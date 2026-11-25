# Pull mode example

Nodes fetch their own configuration from `fleetreeve serve` instead of being
pushed to over SSH. Start with the local demo, then use the steps below for
a real deployment.

## Try it locally

```sh
make build
examples/pull/local-demo.sh
```

No root, no servers: it starts `fleetreeve serve` on localhost and pulls as two
nodes (`node-a`, `node-b`) defined in `inventory.yaml`. It walks through a
dry run, an apply, a second node with different data, drift being
corrected, a rejected impostor, a node enforcing its cached catalog while
the server is down, and the server's fleet status.

## A real deployment

**1. Server.** On a machine with a checkout of your manifests:

```sh
# a certificate for the server's name (or use one from your CA / Let's Encrypt)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 825 \
    -keyout server.key -out server.crt \
    -subj "/CN=fleetreeve.example.com" -addext "subjectAltName=DNS:fleetreeve.example.com"

fleetreeve serve -i inventory.yaml -secret-file server.secret \
    -tls-cert server.crt -tls-key server.key -reports ./reports site.yaml
```

`fleetreeve-server.service` in this directory runs the same thing under systemd.
The secret file is created on first start; guard it like a private key - it
can mint a token for any host.

**2. Enrol nodes.** For nodes reachable over SSH, from the machine holding
the secret (`-ca` is only needed for a self-signed or private-CA certificate):

```sh
fleetreeve enroll -i inventory.yaml -l app -server https://fleetreeve.example.com:8140 \
    -secret-file server.secret -ca server.crt -plan          # preview
fleetreeve enroll -i inventory.yaml -l app -server https://fleetreeve.example.com:8140 \
    -secret-file server.secret -ca server.crt -every 15m
```

On RPM-based nodes, add `-rpm dist/` to install the fleetreeve package first and
run the packaged service instead of a pushed binary:

```sh
fleetreeve enroll -i inventory.yaml -l app -rpm dist/ \
    -server https://fleetreeve.example.com:8140 -secret-file server.secret -ca server.crt
```

For nodes the controller cannot reach, let them install themselves. Mint a
short-lived join token, then run the printed command on the node as root:

```sh
fleetreeve token -secret-file server.secret -ttl 24h -server https://fleetreeve.example.com:8140
# curl -fsSL https://fleetreeve.example.com:8140/install.sh | sh -s -- -server https://fleetreeve.example.com:8140 -join join.…
```

The node downloads the server's fleetreeve build for its platform, verifies it,
and installs the `fleetreeve-pull` service; from then on it also keeps its own
binary in step with the server's. The node's name in the inventory is its
short hostname; pass `-host <name>` when they differ. With a private CA,
get the CA file onto the node first and add `--cacert /path/ca.pem` to curl
and `-ca /path/ca.pem` to the script's arguments.

In a **kickstart** file (CentOS/RHEL). `fleetreeve install` starts a systemd
service, and systemd is not running inside the installer, so `%post` only
plants a one-shot unit that joins on first boot:

```
%post
cat > /etc/systemd/system/fleetreeve-join.service <<'UNIT'
[Unit]
Description=Join fleetreeve on first boot
After=network-online.target
Wants=network-online.target
ConditionPathExists=!/etc/fleetreeve/token

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'curl -fsSL https://fleetreeve.example.com:8140/install.sh | sh -s -- -server https://fleetreeve.example.com:8140 -join join.1795000000.4f6c… -every 15m'

[Install]
WantedBy=multi-user.target
UNIT
systemctl enable fleetreeve-join.service
%end
```

If your kickstart installs the **fleetreeve RPM** from your own repository
(`%packages` … `fleetreeve`), the first-boot unit shrinks to the join itself,
with no download:

```
ExecStart=/usr/bin/fleetreeve install -server https://fleetreeve.example.com:8140 -join join.1795000000.4f6c… -every 15m
```

With **cloud-init** (which runs on a booted system, so the one-liner works
as is):

```yaml
#cloud-config
runcmd:
  - curl -fsSL https://fleetreeve.example.com:8140/install.sh | sh -s -- -server https://fleetreeve.example.com:8140 -join join.1795000000.4f6c… -every 15m
```

A join token baked into a kickstart file or image is readable by anyone who
can read that file, and works until it expires - mint one per build window
rather than a long-lived one. The host must already be in `inventory.yaml`.

For nodes without systemd, do by hand what enrolment does:

```sh
fleetreeve token -secret-file server.secret node-a        # on the server
# on the node: store that in /etc/fleetreeve/token (mode 0600), then
fleetreeve pull -server https://fleetreeve.example.com:8140 -ca /etc/fleetreeve/ca.pem \
    -token-file /etc/fleetreeve/token -every 15m
```

**3. Operate.**

```sh
git pull                                     # in the server's checkout: live on the next check-in
fleetreeve status -reports ./reports -i inventory.yaml       # exit 1 if a node's last run failed
fleetreeve enroll -i inventory.yaml -l node-b -remove        # take a node out of pull mode
```

Removing a host from `inventory.yaml` revokes its access.
