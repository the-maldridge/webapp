package themeblocks

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestBlocksHandlesTagSyntaxVariants covers the whitespace and whitespace-control
// forms pongo2 accepts inside a tag. A pattern matching only the one style this
// repository happens to write would report every block as missing in a theme
// formatted differently, which refuses a correct theme.
func TestBlocksHandlesTagSyntaxVariants(t *testing.T) {
	src := strings.Join([]string{
		`{% block title %}a{% endblock %}`,
		`{%-block head-%}b{%endblock}`,
		`{%block   spaced   %}c{% endblock %}`,
		`{%  block title %}dup{% endblock %}`,
	}, "\n")

	got := Blocks(src)
	want := []string{"title", "head", "spaced"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Blocks() = %v, want %v (duplicates collapsed, order preserved)", got, want)
	}
}

// TestBlocksIgnoresTagsThatAreNotBlocks covers endblock, blockquoted text, and a
// literal mention of the word block in prose. Counting those as declarations would
// hide a genuinely missing block behind a phantom one.
func TestBlocksIgnoresTagsThatAreNotBlocks(t *testing.T) {
	src := `<p>every block of markup below</p>
{% endblock %}
{% if content %}{% endif %}`

	if got := Blocks(src); len(got) != 0 {
		t.Errorf("Blocks() = %v, want nothing", got)
	}
}

// TestMissingReportsTheAbsentBlock is the core of the startup check: a skeleton
// that omits the block its children fill.
func TestMissingReportsTheAbsentBlock(t *testing.T) {
	tree := fstest.MapFS{
		"errors/base.p2": {Data: []byte(`{% block title %}{% endblock %}{% block content %}{% endblock %}`)},
	}

	lines := Missing(tree, map[string][]string{
		"errors/base.p2": {"title", "content", "detail"},
	})
	if len(lines) != 1 || !strings.Contains(lines[0], "errors/base.p2") || !strings.Contains(lines[0], "detail") {
		t.Fatalf("Missing() = %v, want one line naming errors/base.p2 and detail", lines)
	}
}

// TestMissingSkipsAbsentTemplates keeps this check from reporting a theme for a
// file it does not have, which the required-template check already refused it for
// by name and more precisely.
func TestMissingSkipsAbsentTemplates(t *testing.T) {
	lines := Missing(fstest.MapFS{}, Required)
	if len(lines) != 0 {
		t.Errorf("Missing() over an empty tree = %v, want nothing", lines)
	}
}

// TestMissingIsStable covers the error message a person reads. A result built
// straight from map iteration order changes between runs, and a startup error that
// shuffles is one nobody can compare to last week's.
func TestMissingIsStable(t *testing.T) {
	tree := fstest.MapFS{
		"base.p2":        {Data: []byte(`{% block content %}{% endblock %}`)},
		"login.p2":       {Data: []byte(`{% block content %}{% endblock %}`)},
		"errors/base.p2": {Data: []byte(`{% block content %}{% endblock %}`)},
	}

	first := strings.Join(Missing(tree, Required), "\n")
	for range 40 {
		if got := strings.Join(Missing(tree, Required), "\n"); got != first {
			t.Fatalf("Missing() order is not stable:\n%s\n\n%s", first, got)
		}
	}
	if !strings.HasPrefix(first, "base.p2") {
		t.Errorf("want sorted output, got:\n%s", first)
	}
}

// TestRequiredCoversWhatTheFrameworkFills pins the contract to something other
// than itself: the templates the framework renders are required to exist, and the
// skeleton is required to publish the block its children fill, because that is the
// one pairing whose failure pongo2 cannot report.
func TestRequiredCoversWhatTheFrameworkFills(t *testing.T) {
	for file, blocks := range Required {
		if len(blocks) == 0 {
			t.Errorf("Required lists %s with no blocks, which checks nothing", file)
		}
		seen := map[string]bool{}
		for _, b := range blocks {
			if seen[b] {
				t.Errorf("Required lists block %q twice for %s", b, file)
			}
			seen[b] = true
		}
	}

	if got := Required["errors/base.p2"]; !contains(got, "detail") {
		t.Errorf("the error skeleton's required blocks %v omit detail, which is the whole reason "+
			"this check exists", got)
	}
}

// contains reports whether names includes want.
func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
