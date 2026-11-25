#!/bin/sh
# Runs as an RPM scriptlet ($1: how many versions remain) and as a Debian
# maintainer script ($1: remove, upgrade, ...).
case "$1" in
    0|remove) systemctl --no-reload disable --now fleetreeve-server.service >/dev/null 2>&1 || : ;;
esac
exit 0
