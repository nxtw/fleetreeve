# Running the server in a container

The server (`fleetreeve serve`) can run in a container. `make image` (or
`make image CONTAINER=podman`) builds `fleetreeve-server:<version>` from the
`Containerfile`: the static fleetreeve binary and the node builds it hands out,
on an empty base image - no shell, no package manager - running as an
unprivileged user (uid 65532).

```sh
docker run -d --name fleetreeve-server -p 8140:8140 \
    -v /srv/fleetreeve:/srv/fleetreeve:ro \
    -v /etc/fleetreeve/tls:/etc/fleetreeve/tls:ro \
    -v fleetreeve-server-data:/var/lib/fleetreeve-server \
    fleetreeve-server:0.1.0
```

| Mount | Holds |
| --- | --- |
| `/srv/fleetreeve` | your manifests: `site.yaml`, `inventory.yaml` (read-only; edits and `git pull` are live) |
| `/etc/fleetreeve/tls` | `server.crt`, `server.key` - the key must be readable by uid 65532 |
| `/var/lib/fleetreeve-server` | the server secret and the reports. Keep this volume: losing the secret invalidates every node's token |

Logs are JSON on the container's output. Tokens are minted inside the
container, where the secret lives:

```sh
docker exec fleetreeve-server fleetreeve token -secret-file /var/lib/fleetreeve-server/secret \
    -ttl 24h -server https://fleetreeve.example.com:8140
docker exec fleetreeve-server fleetreeve status -reports /var/lib/fleetreeve-server/reports -i inventory.yaml
```

Other arguments replace the default command, e.g. a different manifest or
port: `docker run … fleetreeve-server:0.1.0 serve -i inv.yaml -secret-file … other.yaml`.
[`packaging/container/`](../packaging/container) has a Compose file and a
podman/systemd unit (quadlet) for CentOS/RHEL 9; on SELinux hosts add `:Z`
to bind mounts, as the quadlet does.

If the host runs firewalld, open the server's port and let podman restore
its port forwarding whenever firewalld reloads:

```sh
firewall-cmd --permanent --add-port=8140/tcp && firewall-cmd --reload
systemctl enable --now netavark-firewalld-reload.service
```

This setup was tested with podman 5 on CentOS Stream 9 with SELinux
enforcing, using the quadlet: the image is about 28 MB, the container runs
read-only as uid 65532 with no capabilities, and the secret survives
restarts in its volume.

The image is for the server. Push-mode commands (`apply`, `enroll`) need an
SSH client and your SSH keys, which it deliberately does not contain; run
those from a workstation or CI job with the plain binary or the RPM. To
have them report into the containerised server, copy the secret out once
(`docker cp fleetreeve-server:/var/lib/fleetreeve-server/secret .`) and guard it.
