package ui

import (
	"os"
	"reflect"
	"testing"

	"github.com/projectbluefin/bluefin-report/internal/publish"
)

func TestComposeDraft(t *testing.T) {
	blank := composeForm{Mode: publish.NewIssue, Repo: "projectbluefin/dakota", Title: "  ", Desc: " \n", Steps: "\t"}
	if _, ok := composeDraft("id", blank); ok {
		t.Fatal("an empty form must not be saved")
	}
	d, ok := composeDraft("id", composeForm{
		Mode: publish.NewIssue, Repo: "projectbluefin/dakota", Title: " Wi-Fi drops ", IssueRef: "#4",
		Desc: "It drops\n", Steps: "1. wait", Queue: "3-human-queue", Area: "hardware", Shots: []string{"/a.png"},
	})
	if !ok {
		t.Fatal("a written form must be saved")
	}
	if d.ReportID != "id" || d.Title != "Wi-Fi drops" || d.Body != "It drops\n" || d.Steps != "1. wait" ||
		d.Queue != "3-human-queue" || d.Area != "hardware" || d.IssueRef != "" || len(d.Screenshots) != 1 {
		t.Fatalf("draft = %+v", d)
	}
	d, ok = composeDraft("id", composeForm{
		Mode: publish.Confirm, Repo: "projectbluefin/dakota", Title: "ignored", IssueRef: " #12 ",
		Queue: "3-human-queue", Area: "hardware",
	})
	if ok {
		t.Fatal("an issue link alone is not worth keeping")
	}
	if d.IssueRef != "#12" || d.Title != "" || d.Queue != "" || d.Area != "" {
		t.Fatalf("confirm draft = %+v", d)
	}
}

func TestUnfinishedDraftRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d, _ := composeDraft("abc123", composeForm{Mode: publish.NewIssue, Repo: "projectbluefin/dakota", Title: "T", Desc: "body", Steps: "steps"})
	if _, err := publish.SaveUnfinished(dir, d); err != nil {
		t.Fatal(err)
	}
	d.Body = "body edited"
	if _, err := publish.SaveUnfinished(dir, d); err != nil {
		t.Fatal(err)
	}
	ds, err := publish.Pending(dir)
	if err != nil || len(ds) != 1 {
		t.Fatalf("pending = %v, %v", ds, err)
	}
	if s := ds[0]; s.Stage != publish.Unfinished || s.Draft.Body != "body edited" || s.Draft.Steps != "steps" {
		t.Fatalf("saved = %+v", s)
	}
	if got := stageText(ds[0]); got != "Not finished writing" {
		t.Fatalf("stage text = %q", got)
	}
}

func TestRestoreUnfinishedDraft(t *testing.T) {
	dir := t.TempDir()
	f := composeForm{
		Mode: publish.NewIssue, Repo: "projectbluefin/dakota", Title: "Wi-Fi drops",
		Desc: "It drops", Steps: "1. wait", Queue: "3-human-queue", Area: "hardware",
	}
	d, _ := composeDraft("abc123", f)
	if _, err := publish.SaveUnfinished(dir, d); err != nil {
		t.Fatal(err)
	}
	u := &ui{stateDir: dir}
	s, ok := u.unfinished("abc123")
	if !ok {
		t.Fatal("the saved draft is not found as Unfinished")
	}
	if got := draftForm(s.Draft); !reflect.DeepEqual(got, f) {
		t.Fatalf("restored form = %+v, want %+v", got, f)
	}
	if _, ok := u.unfinished("other"); ok {
		t.Fatal("an unknown id was found")
	}
}

func TestDryRunNeverSavesDraft(t *testing.T) {
	dir := t.TempDir()
	u := &ui{stateDir: dir, dryRun: true}
	u.saveCompose(false) // returns before reading the form, so no widgets are needed
	if es, err := os.ReadDir(dir); err != nil || len(es) != 0 {
		t.Fatalf("dry run wrote %v, %v", es, err)
	}
}
