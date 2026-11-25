#!/bin/sh
# The server runs unprivileged as its own user.
getent group fleetreeve >/dev/null || groupadd -r fleetreeve
getent passwd fleetreeve >/dev/null || \
    useradd -r -g fleetreeve -d /var/lib/fleetreeve-server -s "$(command -v nologin || echo /sbin/nologin)" -c "fleetreeve server" fleetreeve
exit 0
