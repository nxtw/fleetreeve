# Server API

`fleetreeve serve` has an HTTP API for scripts, dashboards and monitoring, on the
same port as the node endpoints. The full reference is
[`api/openapi.yaml`](../api/openapi.yaml).

| | | Token |
| --- | --- | --- |
| `GET /v1/health` | liveness and version | none |
| `GET /v1/status` | latest run of every host and manifest, with a verdict | read |
| `GET /v1/hosts` | hosts the server knows, and where each comes from | read |
| `GET /v1/hosts/{name}` | one host; with the admin token also the variables its catalog is compiled from (they may hold secrets) | read |
| `GET /v1/hosts/{name}/reports/{manifest}` | a stored report, resource by resource | read |
| `PUT /v1/hosts/{name}` | register a host (what `fleetreeve enroll` does) | admin |
| `DELETE /v1/hosts/{name}` | remove it, revoking its access | admin |
| `POST /v1/hosts/{name}/actions` | ask a host to apply now, do a dry run, roll a resource back or release it | operator |
| `POST /v1/hosts/{name}/actions` with `kind: command` | run a shell command on a host, as root | exec |
| `GET /v1/actions` | requests waiting for hosts, and what hosts carried out lately (with command output) | operator |
| `GET /v1/history` | the log of every run and request, newest first; `host`, `only=changes`, `limit`, and `before` to page back | read |
| `DELETE /v1/hosts/{name}/actions/{id}` | withdraw a request the host has not collected | operator |
| `POST /v1/tokens` | the node token of a known host | admin |
| `POST /v1/join-tokens` | a join token for provisioning | admin |

**History.** Reports hold only the latest run of a host. Next to them the
server keeps a log of every run it was told of - the uneventful ones too -
and of every request a host carried out: one JSON document per line, in one
file per day (`<data>/history/2026-11-21.jsonl`, or `-history <dir>`), kept
for 90 days (`-history-days`; 0 keeps everything). A run's entry has its
counts and the resources that were not simply in sync; diffs of file
contents and the text and output of commands are left out, so the log is
safe to read with the read token and to ship elsewhere.

Tokens are bearer tokens derived from the server secret:

```sh
fleetreeve token -secret-file server.secret -api read      # may read
fleetreeve token -secret-file server.secret -api operator  # may also ask hosts to run now and to roll resources back
fleetreeve token -secret-file server.secret -api exec      # may also run shell commands on hosts, as root
fleetreeve token -secret-file server.secret -api admin     # may also change hosts and mint tokens
```

```sh
T=$(fleetreeve token -secret-file server.secret -api read)
curl -s -H "Authorization: Bearer $T" https://fleetreeve.example.com:8140/v1/status
```

```json
{"generated":"…","healthy":true,
 "reports":[{"host":"web1","manifest":"site","source":"pull","mode":"apply","state":"ok",
             "time":"…","age_seconds":41,"interval_seconds":1800,
             "summary":{"total":13,"ok":13,"changed":0,"pending":0,"failed":0,"skipped":0,"refreshed":0,"forgotten":0,"ms":212}}],
 "missing":["db1"]}
```

`state` is `ok`, `changed`, `drift` (a plan found differences), `failed`,
`error` or `stale`; `healthy` is false when any entry is one of the last
three - a single field for a monitoring check. Errors are
`{"error": "..."}` with a matching status code.

`fleetreeve status` uses this API when given a server instead of a reports
directory, so the fleet can be checked from any machine:

```sh
fleetreeve status -server https://fleetreeve.example.com:8140 -secret-file server.secret
FLEETREEVE_API_TOKEN=… fleetreeve status -server https://fleetreeve.example.com:8140      # a read token, no secret needed
```

Things to know:

- Give dashboards and monitoring the **read** token. The **operator** token
  adds asking a host to do its run now, and to roll a resource back to a
  version it kept - nothing a host would not do by itself. The **exec**
  token runs shell commands as root on any host: treat it like a root
  password for the fleet. The **admin** token can mint the token of any
  host, so it is as sensitive as the secret.
- All of them are fixed for a given secret; replacing the secret is the
  only way to revoke them, and that re-keys every node too.
- `GET /v1/hosts/{name}` returns the host's variables, which may include
  whatever secrets your inventory holds.
- Reports contain no content diffs, so the API cannot return file contents.
