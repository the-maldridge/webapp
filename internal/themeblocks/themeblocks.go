// Package themeblocks finds the blocks a theme's templates publish.
//
// It exists because of how pongo2 treats a block a child fills and the parent it
// extends does not declare: the child renders, successfully, with the filled
// block thrown away. No parse error, no render error, and nothing in the output
// that was not also in the output had the block never been written. A theme whose
// error skeleton forgets to declare `detail` therefore produces three error pages
// that render the skeleton and discard the reason, which is the entire content of
// the page, quietly, on the one page that appears when something is already wrong.
//
// Which blocks a theme has to publish is fixed by the framework's own templates,
// because those are the ones that fill them, so the contract is checkable rather
// than a matter of taste. The list lives with the check that uses it, in the core.
package themeblocks

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// blockRe finds the name in a `{% block name %}` tag. Pongo2 permits arbitrary
// whitespace inside the braces and an optional dash for whitespace control, which
// is why the pattern allows a run of either rather than a single space.
var blockRe = regexp.MustCompile(`\{%-?\s*block\s+(\w+)`)

// Required are the blocks each framework layout has to declare.
//
// The rule behind the list is the block surface DESIGN.md documents for these
// templates, because that document is what an application author writes against:
// someone who adds `{% block footer %}` to a page has been told the framework's
// layouts have somewhere to put it. Requiring exactly that surface is what turns
// the documentation from a description into a promise, and anything less lets a
// theme omit a block an application fills and lose that content silently, which is
// the defect this exists to catch. It does not require more than that, which would
// make every theme carry markup nothing renders.
//
// errors/base.p2 carries the weight here. Its three children all fill `detail`,
// and pongo2 drops a filled block whose parent never declared it, with no error at
// parse time or render time. A skeleton that forgot the block therefore renders its
// three error pages with the reason for the failure removed from them, which is the
// entire content of the page, silently, on the page that appears when something is
// already wrong.
var Required = map[string][]string{
	"base.p2":        {"title", "head", "bodyclass", "header", "content", "footer"},
	"login.p2":       {"title", "content"},
	"errors/base.p2": {"title", "content", "detail"},
}

// Blocks returns the block names src declares, in the order they first appear.
func Blocks(src string) []string {
	var (
		found []string
		seen  = map[string]bool{}
	)
	for _, match := range blockRe.FindAllStringSubmatch(src, -1) {
		if seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		found = append(found, match[1])
	}
	return found
}

// Missing returns one line per template whose declared blocks do not cover what
// want demands, sorted so that the message is the same on every run: a startup
// error a person has to diff against last week's is a poor use of a map.
//
// A template listed in want but absent from the tree is skipped, because the
// framework has already refused such a theme by name and more precisely than this
// check can.
func Missing(tree fs.FS, want map[string][]string) []string {
	var lines []string
	for name, blocks := range want {
		src, err := fs.ReadFile(tree, name)
		if err != nil {
			continue
		}

		have := map[string]bool{}
		for _, declared := range Blocks(string(src)) {
			have[declared] = true
		}

		var absent []string
		for _, required := range blocks {
			if !have[required] {
				absent = append(absent, required)
			}
		}
		if len(absent) > 0 {
			sort.Strings(absent)
			lines = append(lines, name+" does not declare "+strings.Join(absent, ", "))
		}
	}
	sort.Strings(lines)
	return lines
}
