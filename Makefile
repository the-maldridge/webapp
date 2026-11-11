# The framework has no generated assets: the alternative theme that used to vendor
# a stylesheet by fetching it now lives in its own repository, where the bundle is
# committed. A fresh clone of this module therefore builds without fetching any
# asset of its own, and the generate target below is left in place only for
# whatever comes next.
GO ?= go

# Modules to check. The example application is a module of its own, because it
# depends on a theme and a theme depends on the framework; listing both here is what
# keeps an example that does not compile from surviving a release.
MODULES := . examples

.PHONY: all generate fmt vet build test race tidy clean

all: fmt vet build test

## generate: vendor third-party assets (network; safe to re-run)
generate:
	for m in $(MODULES); do (cd $$m && $(GO) generate ./...) || exit 1; done

## fmt: rewrite sources with gofmt
fmt:
	gofmt -l -w $(shell for m in $(MODULES); do (cd $$m && $(GO) list -f '{{.Dir}}' ./...); done)

## vet: run go vet
vet:
	for m in $(MODULES); do (cd $$m && $(GO) vet ./...) || exit 1; done

## build: compile everything, including examples
#
# Only main packages are linked, and they go to dist/. Both halves of that are
# load-bearing: this module is libraries alone, and `go build -o` reports an error
# when told to write output for a build with nothing to write; and the example's main
# package lives in a directory named tasks, so a bare `go build ./...` there tries to
# write a binary called "tasks" over its own source directory and fails.
build:
	@for m in $(MODULES); do \
		(cd $$m && mains=$$($(GO) list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./...) && \
			test -z "$$mains" || $(GO) build -o dist/ $$mains) || exit 1; \
	done

## test: run the suite once, without caching
test:
	for m in $(MODULES); do (cd $$m && CGO_ENABLED=1 $(GO) test -count=1 ./...) || exit 1; done

## race: run the suite under the race detector (slow)
race:
	for m in $(MODULES); do (cd $$m && CGO_ENABLED=1 $(GO) test -race -count=1 ./...) || exit 1; done

## tidy: verify go.mod and go.sum need no changes
#
# Every module in the tree, each on its own terms: `go mod tidy` is a per-module
# operation that ignores any workspace, which is what lets the example resolve
# this repository the way its own consumers do, by published version.
tidy:
	for m in $(MODULES); do (cd $$m && $(GO) mod tidy) || exit 1; done

## clean: remove build output
clean:
	rm -rf dist examples/dist
