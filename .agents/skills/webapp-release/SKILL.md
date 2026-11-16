---
name: webapp-release
description: Use when cutting a release of the webapp framework, when tagging a version, when updating CHANGELOG.md for a release, or when re-pinning the example module after a tag.
---

# Cut a release of webapp

The module is `github.com/the-maldridge/webapp`. It is alpha software: the version is
`[major].[minor].[patch]`, and until `v1.0.0` a breaking change may ship under any of
those three positions. Take the number from the person asking for the release instead of
deriving it from semantic versioning, and state the number you cut in the report. What a
release owes its callers whatever the number says is a `CHANGELOG.md` entry that marks
breaking changes and shows the old and new signatures.

Tagging and pushing are both things to do only when asked.

## Procedure

1. Confirm the tree is clean and green: `make all`. This is fmt, vet, build, and test
   across both modules; a root-module `go test` is not sufficient, because
   `examples/` is a separate module and carries the integration tests.
2. Confirm every dependency in `go.mod` resolves to a real tag:
   ```bash
   grep -E 'v[0-9]+\.[0-9]+\.[0-9]+-' go.mod   # a pseudo-version shows up here
   ```
   A `+` suffix or a `-0.yyyymmdd...` pseudo-version means the framework is about to
   publish a public API that names an untagged commit of someone else's repository.
   Tag the dependency (or ask its maintainer to) and re-run `go get <dep>@<tag>`
   before tagging here. Neither the build nor `make all` notices this; only the proxy's
   version list does, so `go list -m -versions <dep>` is the check.
3. Confirm `LICENSE` exists at the repository root. A module published without one is
   "all rights reserved" to anyone who runs `go get`, and the build gives no warning.
4. Move everything under `## Unreleased` in `CHANGELOG.md` into a
   `## v<version> - <date>` section. Mark anything that breaks a caller as
   **Breaking**, and for a signature change include the old and new forms in a table:
   a consumer should be able to fix their code from the changelog alone. Add a short
   `### Added`, `### Changed`, or `### Fixed` for the rest, phrased as what changed for
   the person upgrading rather than which files moved.
5. Leave a fresh empty `## Unreleased` above it.
6. Commit the changelog and the release content.
7. Tag it annotated: `git tag -a v<version> -m "webapp v<version>"`. (`v0.0.1` is a
   lightweight tag; an annotated one carries the release date and who made it, which is
   the only provenance anyone has months later.)
8. Re-pin the example, which resolves the framework by published version rather than
   by the working tree:
   ```bash
   cd examples && go get github.com/the-maldridge/webapp@v<version> && go mod tidy
   ```
   This cannot happen before the tag exists, which is why it is a second commit.
   Confirm with `make vet` that the example still compiles against what was actually
   published.
9. Do not push. Report the tag and leave pushing to the person who asked.

## Checks that pay for themselves

- `go doc -all ./... ` for any exported name that appeared without an example, if the
  release adds public surface.
- Grep `CHANGELOG.md` for identifiers that no longer exist. A changelog naming a method
  that was renamed twice is worse than no changelog.
- If a change alters what a caller must write, `MIGRATING.md` gets a paragraph in the
  same release. It is the document a person hits when the changelog says "breaking".
- `GOWORK=off go vet ./...` inside `examples/`, which proves the example builds against
  the published framework rather than whatever the workspace happens to be pointing at.

The Bulma theme is a separate repository with its own tags (`webapp-bulma`); releasing
this one does not release that one, and an application pinning both may need both
advanced.
