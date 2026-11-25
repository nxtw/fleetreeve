# Container image of the fleetreeve pull-mode server (`fleetreeve serve`).
#
#   make image                                   # or:
#   docker build -f Containerfile -t fleetreeve-server:0.1.0 .
#   podman build -t fleetreeve-server:0.1.0 .
#
# The image holds only the static fleetreeve binary and the node builds it hands
# out; there is no shell or package manager in it. Mount at run time:
#   /srv/fleetreeve             your manifests (site.yaml, inventory.yaml), read-only
#   /etc/fleetreeve/tls         server.crt and server.key, readable by uid 65532
#   /var/lib/fleetreeve-server  secret and reports (a volume; keep it)
# See packaging/container/ for compose and podman/systemd examples.

FROM docker.io/library/golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=0.1.0
# Set by the builder to the architecture the image is built for.
ARG TARGETARCH
# Builds for both node architectures are needed whatever the image's own:
# the server hands them to nodes (install.sh, self-update).
RUN set -e; \
    for arch in amd64 arm64; do \
        CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
            -ldflags "-s -w -X github.com/nxtw/fleetreeve/internal/cli.Version=${VERSION}" \
            -o /out/agents/fleetreeve-linux-$arch ./cmd/fleetreeve; \
    done; \
    cp /out/agents/fleetreeve-linux-${TARGETARCH:-$(go env GOARCH)} /out/fleetreeve; \
    mkdir -p /out/data /out/manifests

FROM scratch
COPY --from=build /out/fleetreeve /usr/bin/fleetreeve
COPY --from=build /out/agents /usr/libexec/fleetreeve/agents
COPY --from=build /out/manifests /srv/fleetreeve
# Owned by the unprivileged user the server runs as, so a fresh named
# volume mounted here is writable.
COPY --from=build --chown=65532:65532 /out/data /var/lib/fleetreeve-server
USER 65532:65532
WORKDIR /srv/fleetreeve
VOLUME /var/lib/fleetreeve-server
EXPOSE 8140
ENTRYPOINT ["/usr/bin/fleetreeve"]
CMD ["serve", "-i", "inventory.yaml", \
     "-secret-file", "/var/lib/fleetreeve-server/secret", \
     "-tls-cert", "/etc/fleetreeve/tls/server.crt", \
     "-tls-key", "/etc/fleetreeve/tls/server.key", \
     "-reports", "/var/lib/fleetreeve-server/reports", \
     "-hosts", "/var/lib/fleetreeve-server/hosts", \
     "-actions", "/var/lib/fleetreeve-server/actions", \
     "site.yaml"]
