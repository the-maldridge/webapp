// The example is an application, and an application is what a theme is for.
//
// It is a module of its own rather than a directory of the framework's module so
// that it can depend on a theme. A theme depends on the framework, so an example
// inside the framework's module that imported a theme would close that graph into
// a cycle between two modules, and the framework would be waiting on its own theme
// to be released before it could be released itself.
module github.com/the-maldridge/webapp/examples

go 1.26.0
