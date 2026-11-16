# Build the plugin for one Gotify release. A Go plugin only loads into the
# server binary it was built for: the same Go version and the same versions of
# every shared module, so the dependencies are capped to the server's go.mod
# and the build runs in the gotify/build image of the server's Go version.
# No -trimpath: the server is built without it, and it changes package hashes.
#
#   make GOTIFY_VERSION=v3.1.1 build

GOTIFY_VERSION ?= v3.1.1
PLUGIN_NAME    := phonelink
BUILDDIR       := dist
VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# every Linux architecture gotify releases (Go plugins do not load on Windows)
ARCHS          ?= linux-amd64 linux-386 linux-arm-7 linux-arm64 linux-riscv64
RAW            := https://raw.githubusercontent.com/gotify/server/$(GOTIFY_VERSION)
DOCKER_RUN      = docker run --rm -v "$$PWD:/proj" -v "$$(go env GOMODCACHE):/go/pkg/mod" -w /proj
GO_BUILD        = go build -mod=readonly -a -installsuffix cgo -buildmode=plugin -ldflags "-X main.Version=$(VERSION)"

.PHONY: build test update-go-mod clean

$(BUILDDIR):
	mkdir -p $(BUILDDIR)

$(BUILDDIR)/gotify-server-$(GOTIFY_VERSION).mod: | $(BUILDDIR)
	curl -fsSL -o $@ $(RAW)/go.mod

$(BUILDDIR)/gotify-go-version-$(GOTIFY_VERSION): | $(BUILDDIR)
	curl -fsSL -o $@ $(RAW)/GO_VERSION

update-go-mod: $(BUILDDIR)/gotify-server-$(GOTIFY_VERSION).mod
	go run github.com/gotify/plugin-api/cmd/gomod-cap@v1.0.0 -from $< -to go.mod
	go mod edit -go=$$(sed -n 's/^go //p' $<) -toolchain=none
	go mod tidy
	go mod download

build: update-go-mod $(BUILDDIR)/gotify-go-version-$(GOTIFY_VERSION)
	@set -e; gov=$$(tr -d '[:space:]' < $(BUILDDIR)/gotify-go-version-$(GOTIFY_VERSION)); \
	for a in $(ARCHS); do \
		echo "== $$a (gotify $(GOTIFY_VERSION), go $$gov)"; \
		if ! docker manifest inspect gotify/build:$$gov-$$a >/dev/null 2>&1; then \
			echo "   skipped: no gotify/build:$$gov-$$a image (gotify did not ship $$a)"; continue; fi; \
		if ! $(DOCKER_RUN) gotify/build:$$gov-$$a $(GO_BUILD) \
			-o $(BUILDDIR)/$(PLUGIN_NAME)-$(GOTIFY_VERSION)-$$a.so /proj 2>$(BUILDDIR)/err; then \
			cat $(BUILDDIR)/err; \
			if grep -q "buildmode=plugin not supported" $(BUILDDIR)/err; then \
				echo "   skipped: go $$gov has no plugin support on $$a"; continue; fi; \
			exit 1; \
		fi; \
	done; rm -f $(BUILDDIR)/err

test:
	go vet ./...
	go test -race ./...

clean:
	rm -rf $(BUILDDIR)
