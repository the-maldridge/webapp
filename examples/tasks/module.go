package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/the-maldridge/webapp"
	"github.com/the-maldridge/webapp/auth"
	"github.com/the-maldridge/webapp/db"
	"gorm.io/gorm"
)

// The tasks feature is a module, so that enabling and disabling it is a
// configuration change rather than an edit to main.
//
// It registers itself in init(), which is what a module's own package does: the
// binary contains the feature, and the application decides which of what it
// linked actually loads. Nothing here is reachable from main.go beyond the
// registration itself.
func init() {
	webapp.Register(webapp.ModuleInfo{
		Name: "tasks",

		// Declared rather than discovered: "what does enabling this module ask
		// to be able to allow?" is answerable from this literal, and two modules
		// cannot claim the same grant without one of them failing to start.
		Permissions: []auth.Perm{permView, permEdit, permDelete, permAdmin},

		Factory: newTasks,
	})
}

// The grants this module owns. TASKS:ADMIN satisfies the others by the
// permission rules, which is why the narrowest sensible grant guards each route
// rather than one broad grant guarding all of them.
var (
	permView   = auth.P("tasks", "view")
	permEdit   = auth.P("tasks", "edit")
	permDelete = auth.P("tasks", "delete")
	permAdmin  = auth.P("tasks", "admin")
)

// templateFS and staticFS are the module's two address spaces. They are embedded
// from this package because a module that ships templates has to have them in
// the binary, and the alternative, a directory read at runtime, means a
// deployment can start with its own templates missing.
//
//go:embed p2
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// tasks is the module's value.
//
// Which of the capability interfaces it happens to implement decides what the
// framework does with it, so shipping a stylesheet was never also an edit to the
// application around it.
type tasks struct {
	app *webapp.App
	db  *db.DB
}

// newTasks builds the module. It reports failure as an error rather than
// returning a nil value with a nil error, which is the specific bug the factory
// signature exists for.
func newTasks(s *webapp.Scope) (any, error) {
	if s.DB() == nil {
		return nil, errors.New("the tasks module needs a database")
	}
	return &tasks{app: s.App(), db: s.DB()}, nil
}

// The capability assertions. A capability the module thinks it has but lost to a
// rename should fail the build rather than quietly stop being routed.
var (
	_ webapp.RouteContributor = (*tasks)(nil)
	_ webapp.Templater        = (*tasks)(nil)
	_ webapp.Staticer         = (*tasks)(nil)
	_ webapp.Navigator        = (*tasks)(nil)
	_ webapp.Component        = (*tasks)(nil)
	_ webapp.Migrator         = (*tasks)(nil)
)

// Routes registers the module's routes.
//
// The framework hands this a router whose middleware cannot leak out of the
// module, which is what makes the Use below a scoping statement rather than a
// global one. Identity is applied to the whole subtree because every route here
// belongs to somebody; the grants are applied per route, because "may see the
// list" and "may delete a row" are different questions with different answers in
// the same account.
func (m *tasks) Routes(r webapp.Router) {
	r.Route("/ui/task", func(r webapp.Router) {
		r.Use(m.app.RequireLogin())

		r.Get("/", m.gated(permView, m.list))
		r.Get("/new", m.gated(permEdit, m.newForm))
		r.Post("/new", m.gated(permEdit, m.newSubmit))
		r.Get("/stats", m.gated(permAdmin, m.stats))

		r.Get("/{id}/edit", m.gated(permEdit, m.editForm))
		r.Post("/{id}/edit", m.gated(permEdit, m.editSubmit))
		r.Post("/{id}/complete", m.gated(permEdit, m.complete))
		r.Post("/{id}/delete", m.gated(permDelete, m.destroy))
	})
}

// gated is a permission check around an error-returning handler. Written once
// here so that every route in the file is the same shape and none of them can
// forget one half of it.
func (m *tasks) gated(p auth.Perm, h webapp.HandlerErr) http.HandlerFunc {
	return m.app.Guarded(p, m.app.WrapErr(h))
}

// Templates mounts this module's templates under its own namespace. Returning
// the module's own name is the normal case; the framework validates it against
// every other mounted root, so a second module called tasks fails at startup
// rather than rendering somebody else's page.
func (m *tasks) Templates() (string, fs.FS) {
	return "tasks", subFS(templateFS, "p2")
}

// Static mounts this module's assets at /static/tasks/, which is what lets it
// ship css/tasks.css without owning that name.
func (m *tasks) Static() (string, fs.FS) {
	return "tasks", subFS(staticFS, "static")
}

// Nav places the module in the navigation. Each item names the same grant the
// route behind it is gated on, which is the only reason a visible link and a
// refused destination cannot disagree.
func (m *tasks) Nav() []webapp.NavItem {
	return []webapp.NavItem{
		{Weight: 80, Label: "Tasks", Href: "/ui/task/", Perm: permView},
		{Weight: 92, Label: "Task totals", Href: "/ui/task/stats", Perm: permAdmin},
	}
}

// Models contributes the table. Naming it here is what puts the table in the
// schema before the first request, including when the module is loaded for the
// first time into a database that already exists.
func (m *tasks) Models() []any { return []any{Task{}} }

// Migrate runs on every boot, after the model has synced.
//
// The index is created by statement rather than by a struct tag so that it
// survives a change to the field that motivated it: an index derived from a tag
// disappears when the tag does, and a list page that was fast until somebody
// renamed a column is a slow surprise rather than a build failure.
func (m *tasks) Migrate(ctx context.Context, d *db.DB) error {
	const stmt = `CREATE INDEX IF NOT EXISTS tasks_owner_state ON tasks (profile_id, done)`
	if err := d.Raw().WithContext(ctx).Exec(stmt).Error; err != nil {
		return fmt.Errorf("create tasks index: %w", err)
	}
	return nil
}

// list renders the viewer's own tasks.
//
// The owner is the profile the authentication layer provisioned, not a query
// parameter, which makes this the line that decides whether the application has
// tenanted data or an open directory.
func (m *tasks) list(w http.ResponseWriter, r *http.Request) error {
	profileID, err := auth.MustProfileIdentity(r)
	if err != nil {
		return err
	}

	page, size, err := webapp.Page(r)
	if err != nil {
		return err
	}
	state := stateFilter(r)

	total, err := m.count(r, uint(profileID), state)
	if err != nil {
		return err
	}
	rows, err := m.owned(r, uint(profileID), state).
		Scopes(db.PaginateScope(page, size)).
		Find(r.Context())
	if err != nil {
		return err
	}

	m.app.View(w, r, "@tasks/list.p2", webapp.Context{
		"tasks":   rows,
		"page":    page,
		"pages":   db.TotalPages(total, size),
		"total":   total,
		"state":   string(state),
		"openURL": "/ui/task/?state=open",
		"doneURL": "/ui/task/?state=done",
		"allURL":  "/ui/task/",
		"prevURL": pageURL(page-1, state),
		"nextURL": pageURL(page+1, state),
	})
	return nil
}

// pageURL builds a pager link. The state rides along because a pager that
// forgets the filter silently changes what is being paged through, and the last
// page of "done" looks exactly like the last page of everything.
func pageURL(page int, state state) string {
	return fmt.Sprintf("/ui/task/?page=%d&state=%s", page, string(state))
}

// owned builds the list query. It is a method because the same conditions have to
// produce both the page of rows and the count behind the pager, and two copies of
// a WHERE clause are two things that can disagree about whose tasks these are.
func (m *tasks) owned(r *http.Request, profileID uint, state state) gorm.ChainInterface[Task] {
	q := db.G[Task](m.db).Where("profile_id = ?", profileID)
	if state != stateAll {
		q = q.Where("done = ?", state == stateDone)
	}
	return q.Order("done asc").Order("created_at desc")
}

// count counts what owned would list. It is a separate query rather than a total
// carried on every row because a pager that says "of 3" over a table holding
// three thousand rows is worse than no pager at all.
func (m *tasks) count(r *http.Request, profileID uint, state state) (int64, error) {
	return m.owned(r, profileID, state).Count(r.Context(), "id")
}

// stats is the module's administrative page and the reason it declares
// TASKS:ADMIN. It counts every account's rows, which is a different kind of
// access from reading a list, and it is the only thing on the page.
func (m *tasks) stats(w http.ResponseWriter, r *http.Request) error {
	handle := m.db.Raw().WithContext(r.Context())

	var total, done int64
	if err := handle.Model(&Task{}).Count(&total).Error; err != nil {
		return err
	}
	if err := handle.Model(&Task{}).Where("done = ?", true).Count(&done).Error; err != nil {
		return err
	}

	var owners int64
	if err := handle.Raw(`SELECT COUNT(DISTINCT profile_id) FROM tasks`).Scan(&owners).Error; err != nil {
		return err
	}

	m.app.View(w, r, "@tasks/stats.p2", webapp.Context{
		"total":  total,
		"done":   done,
		"open":   total - done,
		"owners": owners,
	})
	return nil
}

// newForm and editForm share one template and differ in whether the row exists
// yet.
func (m *tasks) newForm(w http.ResponseWriter, r *http.Request) error {
	m.form(w, r, &Task{}, "/ui/task/new", false, nil)
	return nil
}

func (m *tasks) editForm(w http.ResponseWriter, r *http.Request) error {
	task, err := m.claim(r)
	if err != nil {
		return err
	}
	m.form(w, r, task, m.editURL(task), true, nil)
	return nil
}

// form renders the editor. existed decides the heading and whether the "done"
// checkbox is offered: a task being created is not finished, and a form that let
// you create a completed one is a form that lets you skip the thing it is for.
func (m *tasks) form(w http.ResponseWriter, r *http.Request, task *Task, action string, existed bool, extra webapp.Context) {
	ctx := webapp.Context{
		"task":     task,
		"action":   action,
		"existed":  existed,
		"cancel":   "/ui/task/",
		"maxTitle": maxTitleLength,
		"maxNotes": maxNotesLength,
	}
	for k, v := range extra {
		ctx[k] = v
	}
	m.app.View(w, r, "@tasks/form.p2", ctx)
}

func (m *tasks) newSubmit(w http.ResponseWriter, r *http.Request) error {
	profileID, err := auth.MustProfileIdentity(r)
	if err != nil {
		return err
	}

	title, notes, reasons := readFields(r)
	if len(reasons) > 0 {
		draft := &Task{Title: title, Notes: notes}
		return m.rejected(draft, "/ui/task/new", false, reasons)
	}

	task := &Task{ProfileID: uint(profileID), Title: title, Notes: notes}
	if err := db.G[Task](m.db).Create(r.Context(), task); err != nil {
		return err
	}
	m.app.SeeOther(w, r, "/ui/task/")
	return nil
}

func (m *tasks) editSubmit(w http.ResponseWriter, r *http.Request) error {
	task, err := m.claim(r)
	if err != nil {
		return err
	}

	title, notes, reasons := readFields(r)
	done, doneErr := webapp.FormBool(r, "done")
	if doneErr != nil {
		reasons = append(reasons, "The done flag is not a yes or no.")
	}
	if len(reasons) > 0 {
		draft := *task
		draft.Title, draft.Notes, draft.Done = title, notes, done
		return m.rejected(&draft, m.editURL(task), true, reasons)
	}

	task.Title, task.Notes, task.Done = title, notes, done

	// Save writes the row the form describes. Updating only non-empty fields
	// would make it impossible to delete a note, and a hand-maintained list of
	// columns to update is a list a new column gets forgotten into.
	if err := m.db.Raw().WithContext(r.Context()).Save(task).Error; err != nil {
		return err
	}
	m.app.SeeOther(w, r, "/ui/task/")
	return nil
}

// complete is the one write with no form page of its own, and so the one place a
// checkbox on the list becomes a POST. It flips the stored value rather than
// taking the submitted one: two tabs open on the same task should not be able to
// disagree about what "complete" means.
func (m *tasks) complete(w http.ResponseWriter, r *http.Request) error {
	task, err := m.claim(r)
	if err != nil {
		return err
	}

	rows, err := db.G[Task](m.db).
		Where("id = ?", task.ID).
		Update(r.Context(), "done", !task.Done)
	if err != nil {
		return err
	}
	if rows == 0 {
		return webapp.ErrNotFound
	}
	m.back(w, r, "/ui/task/")
	return nil
}

func (m *tasks) destroy(w http.ResponseWriter, r *http.Request) error {
	task, err := m.claim(r)
	if err != nil {
		return err
	}

	rows, err := db.G[Task](m.db).Where("id = ?", task.ID).Delete(r.Context())
	if err != nil {
		return err
	}
	if rows == 0 {
		return webapp.ErrNotFound
	}
	m.back(w, r, "/ui/task/")
	return nil
}

// claim loads the row the path names and decides whether this caller may act on
// it.
//
// Ownership is checked against the session's profile, so the answer depends on
// nothing in the request body. The failure is a 404 rather than a 403: a stranger
// being told the row exists has been given information nobody offered them, and
// it is the one piece of information every enumeration tool wanted.
//
// TASKS:ADMIN reaches another person's row on purpose, and this is the only place
// that override lives. A module that decides its administrators own everything
// should have to change one function and notice, not audit nineteen handlers for
// the condition they no longer want.
func (m *tasks) claim(r *http.Request) (*Task, error) {
	id, err := webapp.ParamUint(r, "id")
	if err != nil {
		return nil, err
	}

	task, err := db.G[Task](m.db).Where("id = ?", id).First(r.Context())
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, webapp.ErrNotFound
		}
		return nil, err
	}

	if _, err := auth.RequireOwnership(r, task.ProfileID); err == nil {
		return &task, nil
	}
	if auth.HasPerm(r, permAdmin) {
		return &task, nil
	}
	return nil, webapp.ErrNotFound
}

// readFields reads and validates the two text inputs.
//
// The limits are the column sizes. A form that accepts more than the database
// holds turns the user's own typing into a server error, which is both a lie
// about who failed and a 500 nobody can act on.
func readFields(r *http.Request) (title, notes string, reasons []string) {
	title, err := webapp.FormString(r, "title")
	if err != nil {
		return "", "", append(reasons, "The form could not be read.")
	}
	notes, err = webapp.FormString(r, "notes")
	if err != nil {
		return title, "", append(reasons, "The form could not be read.")
	}
	title, notes = strings.TrimSpace(title), strings.TrimSpace(notes)

	if title == "" {
		reasons = append(reasons, "A task needs a title.")
	}
	if n := utf8.RuneCountInString(title); n > maxTitleLength {
		reasons = append(reasons, fmt.Sprintf(
			"That title is %d characters, and %d is the most a task can hold.", n, maxTitleLength))
	}
	if n := utf8.RuneCountInString(notes); n > maxNotesLength {
		reasons = append(reasons, fmt.Sprintf(
			"Those notes are %d characters, and %d is the most they can hold.", n, maxNotesLength))
	}
	return title, notes, reasons
}

// rejected answers a refusal by naming the form to render again with what was
// typed still in it.
//
// This is the whole reason Rejected is a type rather than a status code on a
// generic error: the person submitting needs the page back, and the monitoring
// system needs the 422, and neither of them can be satisfied by an error page.
func (m *tasks) rejected(draft *Task, action string, existed bool, reasons []string) error {
	return &webapp.Rejected{
		Template: "@tasks/form.p2",
		Values: webapp.Context{
			"task":     draft,
			"action":   action,
			"existed":  existed,
			"cancel":   "/ui/task/",
			"maxTitle": maxTitleLength,
			"maxNotes": maxNotesLength,
		},
		Reasons: reasons,
	}
}

// back returns to the list, honouring a next that names a path. The framework's
// own redirect rules check the target first, so a hostile value sends the browser
// to the list rather than to whoever wrote it.
func (m *tasks) back(w http.ResponseWriter, r *http.Request, fallback string) {
	next := strings.TrimSpace(r.FormValue("next"))
	if next == "" {
		next = r.URL.Query().Get("next")
	}
	m.app.SeeOther(w, r, auth.SafeNext(next, fallback))
}

// editURL builds the path a task's form posts to. It is a method so the row's own
// ID is the only input, which is what keeps the "new" case from being able to
// produce an edit URL by forgetting an argument.
func (m *tasks) editURL(task *Task) string { return "/ui/task/" + fmt.Sprint(task.ID) + "/edit" }

// state is which half of the list to show.
type state string

const (
	stateOpen state = "open"
	stateDone state = "done"
	stateAll  state = "all"
)

// stateFilter reads the filter. An unrecognised value means "all" rather than an
// error: a filter that was not understood is a filter that was not applied, and a
// list of the wrong subset is not worth a 400.
func stateFilter(r *http.Request) state {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state"))) {
	case string(stateOpen):
		return stateOpen
	case string(stateDone):
		return stateDone
	default:
		return stateAll
	}
}

// subFS roots an embedded tree at one directory, which is what a namespace wants:
// names like "list.p2" rather than "p2/list.p2".
func subFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		// Unreachable: the embed directives above name these directories
		// literally, so a checkout without them does not build.
		panic("tasks: embedded directory is missing: " + err.Error())
	}
	return sub
}
