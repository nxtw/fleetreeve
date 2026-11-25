# The version is the git tag (v0.2.0 -> 0.2.0), with the distance to it
# and the commit when the checkout is past the tag. Override: make VERSION=...
VERSION  ?= $(shell git describe --tags --match 'v*' --dirty 2>/dev/null | sed 's/^v//' | grep . || echo 0.1.0-dev)
RELEASE  ?= 1
PLATFORMS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64
FLAGS    := -trimpath -ldflags "-s -w -X github.com/nxtw/fleetreeve/internal/cli.Version=$(VERSION)"
NFPM     := go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.1

CONTAINER ?= docker

.PHONY: build agents test install rpm deb packages tarballs release image image-dashboard clean help

help:             ## this list
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed -e 's/:.*##/\t/' | expand -t 18

build:            ## the fleetreeve binary for this machine: bin/fleetreeve
	go build $(FLAGS) -o bin/fleetreeve ./cmd/fleetreeve

agents:           ## builds for every platform, found next to bin/fleetreeve: bin/agents/
	@for p in $(PLATFORMS); do \
		echo "bin/agents/fleetreeve-$$p"; \
		CGO_ENABLED=0 GOOS=$${p%-*} GOARCH=$${p#*-} go build $(FLAGS) -o bin/agents/fleetreeve-$$p ./cmd/fleetreeve || exit 1; \
	done

test:             ## go vet and every test
	go vet ./...
	go test ./...

install: build agents  ## into ~/.local/bin
	install -d $(HOME)/.local/bin/agents
	install bin/fleetreeve $(HOME)/.local/bin/fleetreeve
	install bin/agents/* $(HOME)/.local/bin/agents/

# nfpm makes a package of one kind from a definition in packaging/.
define package
	@mkdir -p dist
	@for arch in amd64 arm64; do for pkg in fleetreeve fleetreeve-server; do \
		sed -e 's/$${VERSION}/$(VERSION)/g' -e 's/$${RELEASE}/$(RELEASE)/g' -e "s/\$${GOARCH}/$$arch/g" \
			packaging/$$pkg.yaml > dist/.$$pkg-$$arch.yaml && \
		$(NFPM) package --config dist/.$$pkg-$$arch.yaml --packager $(1) --target dist/ >/dev/null && \
		rm dist/.$$pkg-$$arch.yaml || exit 1; \
	done; done
	@ls -1 dist/*.$(1)
endef

rpm: agents       ## dist/fleetreeve-*.rpm and dist/fleetreeve-server-*.rpm, for x86_64 and aarch64
	$(call package,rpm)

deb: agents       ## dist/fleetreeve_*.deb and dist/fleetreeve-server_*.deb, for amd64 and arm64
	$(call package,deb)

packages: rpm deb ## both kinds

tarballs: agents  ## dist/fleetreeve_<os>_<arch>.tar.gz: the binary, the builds for hosts, licence and README
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%-*}; arch=$${p#*-}; d=dist/.tar/fleetreeve; rm -rf dist/.tar; mkdir -p $$d/agents; \
		cp bin/agents/fleetreeve-$$p $$d/fleetreeve; cp bin/agents/fleetreeve-linux-* $$d/agents/; \
		cp LICENSE NOTICE README.md CHANGELOG.md $$d/; \
		tar -C $$d -czf dist/fleetreeve_$${os}_$${arch}.tar.gz . || exit 1; \
		echo "dist/fleetreeve_$${os}_$${arch}.tar.gz"; \
	done; rm -rf dist/.tar

release: tarballs packages  ## everything a release carries, with checksums: dist/
	@cd dist && rm -f SHA256SUMS && (sha256sum *.tar.gz *.rpm *.deb 2>/dev/null || shasum -a 256 *.tar.gz *.rpm *.deb) > SHA256SUMS
	@echo dist/SHA256SUMS

image:            ## container image of the server: fleetreeve-server:$(VERSION)  (CONTAINER=podman to use podman)
	$(CONTAINER) build -f Containerfile --build-arg VERSION=$(VERSION) -t fleetreeve-server:$(VERSION) .

image-dashboard:  ## container image of the web console: fleetreeve-dashboard:$(VERSION)
	$(CONTAINER) build -f Containerfile.dashboard --build-arg VERSION=$(VERSION) -t fleetreeve-dashboard:$(VERSION) .

clean:            ## remove bin/ and dist/
	rm -rf bin dist
