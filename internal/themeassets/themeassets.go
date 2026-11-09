// Package themeassets resolves the assets a theme's base layout loads.
//
// It exists as a package rather than as a test helper because the same lookup
// has to run at startup, where there is no testing.T and a missing stylesheet is
// a construction error. A theme's layout linking an asset its tree does not
// contain is invisible in every way a rendered page can be inspected: the page
// answers 200, the markup is correct, and the browser tab shows an unstyled
// document. The only moment anyone learns is a bug report about appearance, and
// the engineer investigating it has already ruled out the templates because the
// templates are visibly fine.
//
// The lookup reads the layout rather than a list of filenames on purpose. A theme
// organises its own assets, so the layout is the only place that knows what a
// given theme needs, and a check that hardcoded one theme's filenames would
// silently pass for every other theme.
package themeassets

import (
	"io/fs"
	"regexp"
	"strings"
)

// linksRe finds the URLs a layout loads from below a given static prefix, as a
// stylesheet or as a script. The capture group is the URL, so the attribute name
// never appears in the result.
var linksRe = regexp.MustCompile(`(?:href|src)="([^"]*)"`)

// Links returns the URLs src loads from below urlPrefix, in the order they
// appear and with duplicates collapsed.
//
// A query string is treated as a cache stamp rather than part of the filename,
// because that is what the framework's own stamping produces, and a check that
// included it would report every stamped asset as missing.
func Links(src, urlPrefix string) []string {
	var (
		found []string
		seen  = map[string]bool{}
	)
	for _, match := range linksRe.FindAllStringSubmatch(src, -1) {
		url := match[1]
		if !strings.HasPrefix(url, urlPrefix) {
			continue
		}
		url, _, _ = strings.Cut(url, "?")
		if seen[url] {
			continue
		}
		seen[url] = true
		found = append(found, url)
	}
	return found
}

// Missing returns the URLs layout loads from templates that static does not
// contain, preserving the URLs as the layout wrote them so the message a caller
// produces names something the reader can find in the file.
//
// The error reports that the layout could not be read at all, which a caller will
// usually excuse: a theme with no base layout is already failing a template check
// that says so more precisely than this one can.
func Missing(templates, static fs.FS, layout, urlPrefix string) ([]string, error) {
	src, err := fs.ReadFile(templates, layout)
	if err != nil {
		return nil, err
	}

	var missing []string
	for _, url := range Links(string(src), urlPrefix) {
		name := strings.TrimPrefix(url, urlPrefix)
		if _, err := fs.Stat(static, name); err != nil {
			missing = append(missing, url)
		}
	}
	return missing, nil
}
