#!/bin/sh
# Runs as an RPM scriptlet ($1: how many versions remain) and as a Debian
# maintainer script ($1: remove, upgrade, ...).
systemctl daemon-reload >/dev/null 2>&1 || :
case "$1" in
    [1-9]*|upgrade) systemctl try-restart fleetreeve-pull.service >/dev/null 2>&1 || : ;;
esac
exit 0
