package builtin_test

import (
	"testing"

	"github.com/the-maldridge/webapp/internal/themetest"
	"github.com/the-maldridge/webapp/theme/builtin"
)

// TestThemeContract runs the rules every theme has to satisfy against this one.
//
// The core checks the same things at startup, for every registered theme, so a
// failure here is a startup failure for every application embedding this
// framework. It is checked here too because a package with no test binary has no
// build that fails, and a broken error page shipped in the default theme is the
// worst version of that defect: it is the code that runs when something else is
// already broken.
func TestThemeContract(t *testing.T) {
	themetest.Check(t, "builtin", builtin.FS(), builtin.Static(), builtin.Required)
}
