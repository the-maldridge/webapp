// The example is an application, and an application is what a theme is for.
//
// It is a module of its own rather than a directory of the framework's module so
// that it can depend on a theme. A theme depends on the framework, so an example
// inside the framework's module that imported a theme would close that graph into
// a cycle between two modules, and the framework would be waiting on its own theme
// to be released before it could be released itself.
module github.com/the-maldridge/webapp/examples

go 1.26.0

require (
	github.com/the-maldridge/webapp v0.0.2
	github.com/the-maldridge/webapp-bulma v0.0.1
	gorm.io/gorm v1.31.2
)

require (
	github.com/flosch/pongo2/v6 v6.1.0 // indirect
	github.com/go-chi/chi/v5 v5.3.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jinzhu/now v1.1.5 // indirect
	github.com/mattn/go-sqlite3 v1.14.22 // indirect
	github.com/meehow/securebytes v0.3.1 // indirect
	github.com/the-maldridge/authware v0.1.7 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	gorm.io/driver/postgres v1.6.3 // indirect
	gorm.io/driver/sqlite v1.6.0 // indirect
)
