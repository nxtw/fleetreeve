#!/bin/sh
# Pull mode end to end on one machine, without root: a server, and two
# "nodes" that are really this machine pulling under different host names.
#
#   examples/pull/local-demo.sh
#
# Everything is written under /tmp/fleetreeve-examples/pull and removed first.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
fleetreeve=${FLEETREEVE:-$here/../../bin/fleetreeve}
[ -x "$fleetreeve" ] || { echo "build fleetreeve first: make build" >&2; exit 1; }

work=/tmp/fleetreeve-examples/pull
port=${PORT:-18140}
rm -rf "$work" && mkdir -p "$work/server"
export FLEETREEVE_STATE_DIR="$work/state"      # keep node state out of your home directory

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
pull() { # pull <host> [options]
    host=$1; shift
    "$fleetreeve" pull -server "http://127.0.0.1:$port" -host "$host" -token-file "$work/server/$host.token" "$@"
}

step "start the server (plain http is fine here: localhost only)"
"$fleetreeve" serve -i "$here/inventory.yaml" -secret-file "$work/server/secret" \
    -listen "127.0.0.1:$port" -reports "$work/server/reports" "$here/site.yaml" 2> "$work/server/log" &
server=$!
trap 'kill $server 2>/dev/null || true' EXIT
sleep 1

step "issue a token per node"
for host in node-a node-b; do
    "$fleetreeve" token -secret-file "$work/server/secret" "$host" > "$work/server/$host.token"
    echo "$host: $(cut -c1-12 "$work/server/$host.token")..."
done

step "node-a: dry run, then apply"
pull node-a -plan || true                 # exit status 2 = changes pending
pull node-a

step "node-b pulls its own catalog (different inventory data)"
pull node-b -no-diff

step "someone edits a file on node-a; the next pull puts it back"
echo "workers = 999" >> "$work/node-a/app.conf"
pull node-a

step "an impostor: node-b's token cannot fetch node-a's catalog"
"$fleetreeve" pull -server "http://127.0.0.1:$port" -host node-a -token-file "$work/server/node-b.token" || true

step "the server goes away; node-a keeps enforcing its cached catalog"
kill $server; wait $server 2>/dev/null || true
rm "$work/node-a/app.conf"
pull node-a -no-diff

step "what the server recorded"
"$fleetreeve" status -reports "$work/server/reports" -i "$here/inventory.yaml"
echo
echo "server log: $work/server/log"
