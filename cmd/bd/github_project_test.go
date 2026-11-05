package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/steveyegge/beads/internal/types"
)

// fakeProjectSource is a project hierarchy held in memory.
type fakeProjectSource struct {
	records  map[string]*projectRecord
	children map[string][]string
}

func (f fakeProjectSource) Record(_ context.Context, id string) (*projectRecord, error) {
	if r, ok := f.records[id]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("no such record")
}

func (f fakeProjectSource) Descendants(_ context.Context, id string) ([]string, error) {
	var out []string
	queue := []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range f.children[cur] {
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out, nil
}

func item(id, title string, labels ...string) *types.Issue {
	return &types.Issue{ID: id, Title: title, Labels: labels, IssueType: types.TypeTask, Status: types.StatusOpen}
}

func ids(issues []*types.Issue) string {
	var out []string
	for _, i := range issues {
		out = append(out, i.ID)
	}
	return strings.Join(out, ",")
}

func TestResolveProjectRepository(t *testing.T) {
	type want struct{ repo, source string }
	cases := []struct {
		name string
		rec  projectRecord
		want want
		err  string // substring of the error, "" for success
	}{
		{"metadata owner/name", projectRecord{ID: "p-1", Metadata: map[string]any{"repo": "trillium/brain"}}, want{"trillium/brain", "metadata repo"}, ""},
		{"metadata github url", projectRecord{ID: "p-1", Metadata: map[string]any{"repo": "https://github.com/trillium/brain.git"}}, want{"trillium/brain", "metadata repo"}, ""},
		{"metadata github_repo", projectRecord{ID: "p-1", Metadata: map[string]any{"github_repo": "acme/widgets"}}, want{"acme/widgets", "metadata github_repo"}, ""},
		{"repo label", projectRecord{ID: "p-1", Labels: []string{"stack:go", "repo:acme/widgets"}}, want{"acme/widgets", "label repo:acme/widgets"}, ""},
		{"external ref url", projectRecord{ID: "p-1", ExternalRef: "https://github.com/acme/widgets"}, want{"acme/widgets", "external_ref"}, ""},
		{"origin line in description", projectRecord{ID: "p-1", Description: "Found by repo scan. origin github.com/trillium/brain-dashboard; local clone /x/y; license NONE."}, want{"trillium/brain-dashboard", "description (origin line)"}, ""},
		{"metadata wins over description", projectRecord{ID: "p-1", Metadata: map[string]any{"repo": "acme/new"}, Description: "origin github.com/acme/old"}, want{"acme/new", "metadata repo"}, ""},
		{"a local path is skipped, the next place is used", projectRecord{ID: "p-1", Metadata: map[string]any{"repo": "/Users/x/code/brain"}, Description: "origin github.com/trillium/brain"}, want{"trillium/brain", "description (origin line)"}, ""},
		{"same repo twice in one place is fine", projectRecord{ID: "p-1", Metadata: map[string]any{"repo": "Acme/Widgets", "github_repo": "acme/widgets"}}, want{"acme/widgets", ""}, ""},
		{"two different repos in one place is an error", projectRecord{ID: "p-1", Metadata: map[string]any{"repo": "acme/a", "github_repo": "acme/b"}}, want{}, "more than one repository"},
		{"no repository names where to put one", projectRecord{ID: "p-1", Title: "brain"}, want{}, "projects update p-1 --set-metadata repo=owner/name"},
		{"a local path alone is reported as ignored", projectRecord{ID: "p-1", Metadata: map[string]any{"repo": "/Users/x/code/kashukihound"}}, want{}, "ignored, not a GitHub repository"},
		{"a bare name is not a repository", projectRecord{ID: "p-1", Labels: []string{"repo:trillium_talon"}}, want{}, "names no GitHub repository"},
		{"ssh remotes are not guessed", projectRecord{ID: "p-1", Metadata: map[string]any{"repo": "git@github.com:acme/widgets.git"}}, want{}, "names no GitHub repository"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.rec
			owner, repo, source, err := resolveProjectRepository(&rec)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := strings.ToLower(owner + "/" + repo); got != strings.ToLower(tc.want.repo) {
				t.Fatalf("repo = %s, want %s", got, tc.want.repo)
			}
			if tc.want.source != "" && source != tc.want.source {
				t.Fatalf("source = %q, want %q", source, tc.want.source)
			}
		})
	}
}

func newTestScope(t *testing.T, extra ...string) *projectScope {
	t.Helper()
	src := fakeProjectSource{
		records: map[string]*projectRecord{
			"project-1": {ID: "project-1", Title: "brain", Metadata: map[string]any{"repo": "acme/widgets", "project_labels": "project:brain-alias"}},
		},
		children: map[string][]string{
			"project-1":   {"project-1.1", "task-9"},
			"project-1.1": {"task-10"},
		},
	}
	scope, err := loadProjectScope(context.Background(), src, "project-1", extra)
	if err != nil {
		t.Fatalf("loadProjectScope: %v", err)
	}
	return scope
}

func TestProjectScopeSelectsExactlyTheProjectsItems(t *testing.T) {
	scope := newTestScope(t)
	if scope.Repository() != "acme/widgets" {
		t.Fatalf("repository = %q", scope.Repository())
	}
	ephemeral := item("task-eph", "wisp", "project:project-1")
	ephemeral.Ephemeral = true
	gate := item("task-gate", "gate", "project:project-1")
	gate.IssueType = types.TypeGate
	all := []*types.Issue{
		item("task-9", "child of the project record"),
		item("task-10", "grandchild of the project record"),
		item("task-11", "tied by the record id label", "project:project-1"),
		item("task-12", "tied by a child record id label", "project:project-1.1"),
		item("task-13", "tied by the alias the record declares", "Project:Brain-Alias"),
		item("task-14", "not the project's: a private client item", "client-project", "project:portageforward"),
		item("task-15", "not the project's: the title says project-1 and brain"),
		item("task-16", "not the project's: a label that only starts the same", "project:project-12"),
		item("task-17", "not the project's: an unrelated record label", "project-1"),
		item("project-1", "the record itself", "project:project-1"),
		ephemeral,
		gate,
	}
	members, reasons := scope.Select(all)
	if got, want := ids(members), "task-9,task-10,task-11,task-12,task-13"; got != want {
		t.Fatalf("members = %s, want %s", got, want)
	}
	for id, wantReason := range map[string]string{
		"task-9":  "under project-1",
		"task-11": "label project:project-1",
		"task-12": "label project:project-1.1",
		"task-13": "label project:brain-alias",
	} {
		if reasons[id] != wantReason {
			t.Errorf("reason for %s = %q, want %q", id, reasons[id], wantReason)
		}
	}
}

func TestProjectScopeExtraLabelsAddToTheRecordsOwn(t *testing.T) {
	scope := newTestScope(t, "brain", "project:Other")
	all := []*types.Issue{
		item("task-1", "slug label", "project:brain"),
		item("task-2", "explicit label", "project:other"),
		item("task-3", "alias from the record", "project:brain-alias"),
		item("task-4", "no tie"),
	}
	members, _ := scope.Select(all)
	if got, want := ids(members), "task-1,task-2,task-3"; got != want {
		t.Fatalf("members = %s, want %s", got, want)
	}
}

func TestProjectScopeNothingBelongsMeansNothingSelected(t *testing.T) {
	src := fakeProjectSource{records: map[string]*projectRecord{"project-9": {ID: "project-9", Metadata: map[string]any{"repo": "acme/w"}}}}
	scope, err := loadProjectScope(context.Background(), src, "project-9", nil)
	if err != nil {
		t.Fatal(err)
	}
	members, _ := scope.Select([]*types.Issue{item("task-1", "private thing"), item("task-2", "another")})
	if len(members) != 0 {
		t.Fatalf("a project with no items selected %s", ids(members))
	}
}

func TestLoadProjectScopeErrors(t *testing.T) {
	src := fakeProjectSource{records: map[string]*projectRecord{"project-2": {ID: "project-2", Title: "no repo here"}}}
	ctx := context.Background()

	if scope, err := loadProjectScope(ctx, src, "  ", nil); scope != nil || err == nil {
		t.Fatalf("empty id: scope=%v err=%v", scope, err)
	}
	if scope, err := loadProjectScope(ctx, src, "project-404", nil); scope != nil || err == nil || !strings.Contains(err.Error(), "project-404") {
		t.Fatalf("missing record: scope=%v err=%v", scope, err)
	}
	// A record with no repository still yields a scope, so a dry run can name
	// --repo, but it reports the problem.
	scope, err := loadProjectScope(ctx, src, "project-2", nil)
	if scope == nil || err == nil || !strings.Contains(err.Error(), "names no GitHub repository") {
		t.Fatalf("no repo: scope=%v err=%v", scope, err)
	}
	if scope.Repository() != "" {
		t.Fatalf("repository guessed: %q", scope.Repository())
	}
}

func TestSQLProjectSourceReadsTheRecordAndItsTree(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	mock.ExpectQuery("SELECT id, title, description, external_ref, metadata FROM issues WHERE id = ?").
		WithArgs("project-73bi.1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "external_ref", "metadata"}).
			AddRow("project-73bi.1", "brain", nil, nil, `{"repo":"trillium/brain","brain_slug":"x"}`))
	mock.ExpectQuery("SELECT label FROM labels WHERE issue_id = ?").WithArgs("project-73bi.1").
		WillReturnRows(sqlmock.NewRows([]string{"label"}).AddRow("stack:go"))
	src := sqlProjectSource{db: db}
	rec, err := src.Record(ctx, "project-73bi.1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Metadata["repo"] != "trillium/brain" || len(rec.Labels) != 1 {
		t.Fatalf("record = %+v", rec)
	}

	// The tree is walked through parent-child edges, whatever store a child is in.
	mock.ExpectQuery("SELECT issue_id FROM dependencies WHERE depends_on_issue_id = \\? AND type = \\?").
		WithArgs("project-73bi.1", "parent-child").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("task-a").AddRow("fleet-b"))
	mock.ExpectQuery("SELECT issue_id FROM dependencies WHERE depends_on_issue_id = \\? AND type = \\?").
		WithArgs("task-a", "parent-child").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}).AddRow("brain-c"))
	mock.ExpectQuery("SELECT issue_id FROM dependencies WHERE depends_on_issue_id = \\? AND type = \\?").
		WithArgs("fleet-b", "parent-child").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}))
	mock.ExpectQuery("SELECT issue_id FROM dependencies WHERE depends_on_issue_id = \\? AND type = \\?").
		WithArgs("brain-c", "parent-child").
		WillReturnRows(sqlmock.NewRows([]string{"issue_id"}))
	got, err := src.Descendants(ctx, "project-73bi.1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "task-a,fleet-b,brain-c" {
		t.Fatalf("descendants = %v", got)
	}

	mock.ExpectQuery("SELECT id, title, description, external_ref, metadata FROM issues WHERE id = ?").
		WithArgs("project-missing").WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "external_ref", "metadata"}))
	if _, err := src.Record(ctx, "project-missing"); err == nil {
		t.Fatal("a missing record should be an error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
