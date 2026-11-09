# The framework's assets are vendored rather than committed, so on a fresh
# clone the build fails until `make generate` has run. `make all` puts them in
# the right order; the individual targets do not, because a target that quietly
# regenerated a 660 KB stylesheet on every invocation would make every build
# depend on the network.
GO ?= go

# Packages to check. examples/ is included so an example that does not compile
# cannot survive a release.
PKGS := ./...

.PHONY: all generate fmt vet build test race tidy clean

all: generate fmt vet build test

## generate: vendor third-party assets (network; safe to re-run)
generate:
	$(GO) generate $(PKGS)

## fmt: rewrite sources with gofmt
fmt:
	gofmt -l -w $(shell $(GO) list -f '{{.Dir}}' $(PKGS))

## vet: run go vet
vet:
	$(GO) vet $(PKGS)

## build: compile everything, including examples
build:
	$(GO) build $(PKGS)

## test: run the suite once, without caching
test:
	CGO_ENABLED=1 $(GO) test -count=1 $(PKGS)

## race: run the suite under the race detector (slow)
race:
	CGO_ENABLED=1 $(GO) test -race -count=1 $(PKGS)

## tidy: verify go.mod and go.sum need no changes
tidy:
	$(GO) mod tidy

## clean: remove generated assets, leaving the directory in place
clean:
	rm -f theme/bulma/static/css/bulma.min.css theme/bulma/static/css/LICENSE
