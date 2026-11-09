package themeassets

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

// TestLinksIgnoresWhatTheFrameworkDoesNotServe covers the URLs that must not be
// checked: an external stylesheet, a link to an application's own route, and a
// module's asset below a different prefix.
//
// Refusing a theme for a link the framework never claimed to serve would be worse
// than the defect the check guards, because a theme that uses a CDN or links one
// of its application's pages is legitimate and has no way to satisfy such a rule.
func TestLinksIgnoresWhatTheFrameworkDoesNotServe(t *testing.T) {
	const src = `<head>
<link rel="stylesheet" href="https://cdn.example.com/thing.css">
<link rel="stylesheet" href="/static/other/css/module.css">
<a href="/ui/task/">tasks</a>
<link rel="stylesheet" href="/static/core/css/theme.css">
</head>`

	got := Links(src, "/static/core/")
	if len(got) != 1 || got[0] != "/static/core/css/theme.css" {
		t.Errorf("Links() = %v, want only the URL below the framework's prefix", got)
	}
}

// TestLinksStripsCacheStamp covers the query string a stamped URL carries.
func TestLinksStripsCacheStamp(t *testing.T) {
	got := Links(`<link href="/static/core/css/a.css?v=1">`, "/static/core/")
	if len(got) != 1 || got[0] != "/static/core/css/a.css" {
		t.Errorf("Links() = %v, want the stamp removed so the file can be found", got)
	}
}

// TestLinksCollapsesDuplicates keeps one renamed asset from producing the same
// complaint twice, which is noise in an error message a person reads once.
func TestLinksCollapsesDuplicates(t *testing.T) {
	got := Links(`<link href="/static/core/css/a.css"><script src="/static/core/css/a.css"></script>`,
		"/static/core/")
	if len(got) != 1 {
		t.Errorf("Links() = %v, want one entry", got)
	}
}

// TestMissingNamesWhatIsAbsent is the core of the startup check: an asset the
// layout links and the tree lacks, reported as the layout wrote it.
func TestMissingNamesWhatIsAbsent(t *testing.T) {
	templates := fstest.MapFS{
		"base.p2": {Data: []byte(`<link href="/static/core/css/present.css">
<script src="/static/core/css/absent.js"></script>`)},
	}
	static := fstest.MapFS{"css/present.css": {Data: []byte("body{}")}}

	missing, err := Missing(templates, static, "base.p2", "/static/core/")
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if len(missing) != 1 || missing[0] != "/static/core/css/absent.js" {
		t.Errorf("Missing() = %v, want the one absent URL", missing)
	}
}

// TestMissingReportsUnreadableLayout lets a caller distinguish "no assets to
// check" from "the layout is not there", which the core excuses because a more
// precise template check already refused that theme.
func TestMissingReportsUnreadableLayout(t *testing.T) {
	_, err := Missing(fstest.MapFS{}, fstest.MapFS{}, "base.p2", "/static/core/")
	if err == nil {
		t.Fatal("a theme with no base layout reported no problem")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("want a not-exist error, got %v", err)
	}
}
