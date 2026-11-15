---
name: webapp-release
description: Use when cutting a release of the webapp framework, when tagging a version, when updating CHANGELOG.md for a release, or when re-pinning the example module after a tag.
---

# Cut a release of webapp

The module is `github.com/the-maldridge/webapp`. Versions follow semantic
versioning with one stated exception, recorded at the top of `CHANGELOG.md`: before
`v1.0.0` a breaking change bumps the minor, so the release carrying a breaking change
after `v0.0.1` is `v0.1.0`.

Tagging and pushing are both things to do only when asked.

## Procedure

1. Confirm the tree is clean and green: `make all`. This is fmt, vet, build, and test
   across both modules; a root-module `go test` is not sufficient, because
   `examples/` is a separate module and carries the integration tests.
2. Move everything under `## Unreleased` in `CHANGELOG.md` into a
   `## v<version> - <date>` section. Mark anything that breaks a caller as
   **Breaking**, and for a signature change include the old and new forms in a table:
   a consumer should be able to fix their code from the changelog alone. Add a short
   `### Added`, `### Changed`, or `### Fixed` for the rest, phrased as what changed for
   the person upgrading rather than which files moved.
3. Leave a fresh empty `## Unreleased` above it.
4. Commit the changelog and the release content.
5. Tag it: `git tag v<version>`.
6. Re-pin the example, which resolves the framework by published version rather than
   by the working tree:
   ```bash
   cd examples && go get github.com/the-maldridge/webapp@v<version> && go mod tidy
   ```
   This cannot happen before the tag exists, which is why it is a second commit.
   Confirm with `make vet` that the example still compiles against what was actually
   published.
7. Do not push. Report the tag and leave pushing to the person who asked.

## Checks that pay for themselves

- `go doc -all ./... ` for any exported name that appeared without an example, if the
  release adds public surface.
- Grep `CHANGELOG.md` for identifiers that no longer exist. A changelog naming a method
  that was renamed twice is worse than no changelog.
- If a change alters what a caller must write, `MIGRATING.md` gets a paragraph in the
  same release. It is the document a person hits when the changelog says "breaking".

The Bulma theme is a separate repository with its own tags (`webapp-bulma`); releasing
this one does not release that one, and an application pinning both may need both
advanced.
