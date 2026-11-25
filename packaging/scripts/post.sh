#!/bin/sh
# Make systemd see new or changed unit files.
systemctl daemon-reload >/dev/null 2>&1 || :
