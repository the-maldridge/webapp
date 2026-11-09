package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The rules a stylesheet has follow when nothing else is loading one first.
//
// An application's CSS in a themed framework is in a position a stylesheet inside
// an application normally is not: the page it lands on may come from any of
// several themes, and each defines its own variables, its own class names, and
// its own opinions about what a button is. The failure is invisible from a
// screenshot, because the page looks right in whichever theme it was written
// against and arrives with no colours in every other one.

// declarationRe matches a custom property being defined, as the only thing on a
// line inside a rule.
var declarationRe = regexp.MustCompile(`^\s*(--[a-z0-9]+(?:-[a-z0-9]+)*)\s*:`)

// useRe matches a custom property being consumed, with or without a fallback.
var useRe = regexp.MustCompile(`var\(\s*(--[a-z0-9]+(?:-[a-z0-9]+)*)\s*[,)]`)

// TestStylesheetDeclaresEveryPropertyItConsumes reads the module's own sheet and
// fails on a variable it uses without declaring.
//
// The rule is "declare what you consume", and the reason it is checkable rather
// than conventional is that CSS resolves an undefined custom property by
// substituting nothing, which for a colour leaves the element in whatever colour
// it inherited and for a length leaves a declaration invalid. Neither is an error
// a browser reports in a way anyone looking at a page would connect to a variable.
func TestStylesheetDeclaresEveryPropertyItConsumes(t *testing.T) {
	const sheet = "static/css/tasks.css"

	src, err := os.ReadFile(sheet)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	declared := map[string]bool{}
	for _, line := range strings.Split(string(src), "\n") {
		if m := declarationRe.FindStringSubmatch(line); m != nil {
			declared[m[1]] = true
		}
	}

	missing := map[string]bool{}
	for _, m := range useRe.FindAllStringSubmatch(string(src), -1) {
		if !declared[m[1]] {
			missing[m[1]] = true
		}
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		t.Errorf("%s consumes custom properties it never declares: %v; the framework publishes none "+
			"for an application to borrow, so a name like this is either a typo or a variable that "+
			"belongs to a theme and will vanish when the theme changes", sheet, names)
	}
}

// TestStylesheetIsNotTheOnlyOneAssumed asserts the module's sheet styles nothing
// through a bare element selector.
//
// A rule for h1 in a module's sheet is an opinion about every other module's
// headings, which is the failure a URL namespace cannot protect against because
// the cascade has no namespace. The check is deliberately crude, matching top-
// level rules whose selector is a single element name: it is here to keep the
// example honest about the rule rather than to be a general CSS parser.
func TestStylesheetIsNotTheOnlyOneAssumed(t *testing.T) {
	const sheet = "static/css/tasks.css"

	src, err := os.ReadFile(sheet)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Strip comments so the prose at the top of the file, which names elements
	// while explaining why it does not style them, is not read as a rule.
	body := commentRe.ReplaceAllString(string(src), "")

	var offenders []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, "{") {
			continue
		}
		selector := strings.TrimSuffix(line, "{")
		if bareElementRe.MatchString(selector) {
			offenders = append(offenders, selector)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%s styles bare elements: %v; a module's sheet styles its own classes and leaves "+
			"elements to the theme, because otherwise every theme's typography is negotiated by load order",
			sheet, offenders)
	}
}

var (
	commentRe     = regexp.MustCompile(`(?s)/\*.*?\*/`)
	bareElementRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
)
