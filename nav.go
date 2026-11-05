package webapp

import (
	"net/http"
	"sort"
	"strings"

	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/tmpl"
)

// NavItem is one entry in the application's navigation.
//
// It is registered by the application and by its modules rather than being
// written into a template, because navigation is the one piece of chrome that
// has to reflect what the viewer may reach: a hand-written menu is a list of
// links that some viewers cannot use, and the page cannot know that without
// the authorization layer, which the template must not call directly.
type NavItem struct {
	// Weight orders the item. Higher sorts first, and ties break
	// alphabetically, so the order of Nav calls cannot change what a page
	// looks like.
	//
	// The scale is deliberately coarse: 90 and above is administration,
	// 50 through 89 are the primary sections, and below 50 is everything
	// auxiliary. A module author should be able to place themselves without
	// knowing what else is installed, which a dense numbering scheme makes
	// impossible.
	Weight int

	// Label is the visible text.
	Label string

	// Href is the target. The active item is decided by its first path
	// segment, so a module mounted at /tasks and a page at /tasks/42 agree
	// without either declaring anything.
	Href string

	// Icon is an optional short glyph, rendered with aria-hidden so a
	// screen reader gets the label and not the picture of a folder.
	Icon string

	// Perm hides the item from viewers who do not hold it. The zero value
	// names no permission, which means the item is for everyone including
	// the anonymous — the right default for a home page and the wrong one
	// for anything else, so a module that leaves it unset is saying out loud
	// that the link is public.
	Perm auth.Perm

	// Children are nested items. They inherit nothing: each states its own
	// permission, because a parent that is visible is not evidence that
	// everything below it is.
	Children []NavItem
}

// Nav registers navigation entries.
//
// Registration order does not matter; weight and label decide the order shown.
// That is what lets a module place itself next to the things it belongs to
// without reading the application's source to find out what is already there.
func (a *App) Nav(items ...NavItem) {
	a.navItems = append(a.navItems, items...)
}

// navEntry is a navigation item as a template sees it.
//
// The distinction from NavItem is Base, derived from Href here so the template
// compares two path segments rather than parsing a URL, and so the rule for
// "which item is this page" lives in exactly one place. A template doing that
// string work would get it subtly different in the two places it appears, which
// is how a page ends up highlighting nothing while the section above it is
// highlighted twice.
type navEntry struct {
	Label    string
	Href     string
	Icon     string
	Base     string
	Children []navEntry
}

// navFor returns the entries this request may see, in display order.
//
// Filtering happens per request rather than at registration, because the answer
// depends on who is asking: the item a module registers once is visible to an
// administrator and absent for everyone else, and a registration-time decision
// could only be made by pretending the viewer were one of them.
func navFor(r *http.Request, items []NavItem) []navEntry {
	entries := make([]navEntry, 0, len(items))
	for _, item := range items {
		if !navVisible(r, item.Perm) {
			continue
		}
		entry := navEntry{
			Label: item.Label,
			Href:  item.Href,
			Icon:  item.Icon,
			Base:  navBase(item.Href),
		}
		if children := navFor(r, item.Children); len(children) > 0 {
			entry.Children = children
		}
		entries = append(entries, entry)
	}
	return entries
}

// navVisible reports whether to show an item.
//
// The zero permission means "no gate", and it is the only case that does not
// consult the authorization layer. Everything else goes through auth, so there
// is one answer to "may this viewer see this" and hiding a link and refusing
// the route behind it cannot drift apart.
func navVisible(r *http.Request, p auth.Perm) bool {
	if p.Module == "" && p.Grant == "" {
		return true
	}
	return auth.HasPerm(r, p)
}

// navBase is the first path segment of a link, which is what pageBase is
// compared against.
func navBase(href string) string {
	href = strings.Trim(href, "/")
	if href == "" {
		return ""
	}
	if i := strings.IndexByte(href, '/'); i >= 0 {
		return href[:i]
	}
	return href
}

// sortNavItems orders by weight descending, then label ascending.
func sortNavItems(items []NavItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Weight != items[j].Weight {
			return items[i].Weight > items[j].Weight
		}
		return items[i].Label < items[j].Label
	})
}

// installNav shares the navigation and the permission check with templates.
//
// Both are injections rather than shared providers because both need the
// request: navigation depends on who is asking, and a provider receives a
// context, which is the wrong shape for a decision the authorization layer
// makes about a request.
func (a *App) installNav() {
	a.inject(func(r *http.Request, ctx Context) {
		items := make([]NavItem, len(a.navItems))
		copy(items, a.navItems)
		sortNavItems(items)
		ctx["navElements"] = navFor(r, items)

		// can("MODULE:GRANT") in a template. It is a per-request callable
		// rather than a filter because a filter lives in a process-global
		// registry and cannot legitimately reach the current request; a
		// closure built for this request and placed in this request's context
		// has exactly one request it can be wrong about.
		ctx["can"] = tmpl.Perms(func(perm string) bool {
			p, err := auth.ParsePerm(perm)
			if err != nil {
				// A malformed name in a template asks about nothing, and
				// nothing is not a permission. It renders as denied rather
				// than as an error, because a typo in a template would
				// otherwise take down every page that mentions it, and the
				// honest failure a developer wants here is a link that never
				// appears for anyone.
				a.log().WarnContext(r.Context(), "template asked about an invalid permission",
					"permission", perm, "request_id", a.requestID(r))
				return false
			}
			return auth.HasPerm(r, p)
		})
	})
}
