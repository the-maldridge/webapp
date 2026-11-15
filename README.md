# A Webapp Framework

This is basically all the components of a bunch of my opinions in
web-app design with Go, Gorm, and Chi all factored out into a
re-usable library.  This is useful for the kinds of applications that
I build, but probably not as generally consumable as more established
frameworks.  Phrased differently, this is my concept of Django, but in
Go, i.e., a framework with all the basic stuff already ready to go.

## Getting around

| If you want | Go to |
| --- | --- |
| Signatures and documented call shapes | `go doc github.com/the-maldridge/webapp`, and likewise `.../webapp/auth`, `.../db`, `.../tmpl`, `.../webapptest` |
| A task to start on, and the commands that have to pass | `AGENTS.md` |
| Why a decision went the way it did | `DESIGN.md`, indexed from `AGENTS.md` |
| A complete application built on this | `examples/tasks` |
| To move an existing `net/http` or `authware` application over | `MIGRATING.md` |
| What changed between versions | `CHANGELOG.md` |

The example application is a module of its own, and the Bulma theme is a separate
repository, `github.com/the-maldridge/webapp-bulma`, that imports this one.

```bash
make all        # fmt, vet, build, test, across both modules
CGO_ENABLED=1 go test -count=1 ./...   # one module; SQLite needs cgo
```
