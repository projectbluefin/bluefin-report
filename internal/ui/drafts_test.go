package ui

import (
	"testing"

	"github.com/projectbluefin/bluefin-report/internal/publish"
)

func TestComposeDraft(t *testing.T) {
	if _, ok := composeDraft("id", publish.NewIssue, "projectbluefin/dakota", "  ", "", " \n", "\t", "", "", nil); ok {
		t.Fatal("an empty form must not be saved")
	}
	d, ok := composeDraft("id", publish.NewIssue, "projectbluefin/dakota", " Wi-Fi drops ", "#4", "It drops\n", "1. wait", "3-human-queue", "hardware", []string{"/a.png"})
	if !ok {
		t.Fatal("a written form must be saved")
	}
	if d.ReportID != "id" || d.Title != "Wi-Fi drops" || d.Body != "It drops\n" || d.Steps != "1. wait" ||
		d.Queue != "3-human-queue" || d.Area != "hardware" || d.IssueRef != "" || len(d.Screenshots) != 1 {
		t.Fatalf("draft = %+v", d)
	}
	d, ok = composeDraft("id", publish.Confirm, "projectbluefin/dakota", "ignored", " #12 ", "", "", "3-human-queue", "hardware", nil)
	if ok {
		t.Fatal("an issue link alone is not worth keeping")
	}
	if d.IssueRef != "#12" || d.Title != "" || d.Queue != "" || d.Area != "" {
		t.Fatalf("confirm draft = %+v", d)
	}
}

func TestUnfinishedDraftRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d, _ := composeDraft("abc123", publish.NewIssue, "projectbluefin/dakota", "T", "", "body", "steps", "", "", nil)
	if err := publish.SaveUnfinished(dir, d); err != nil {
		t.Fatal(err)
	}
	d.Body = "body edited"
	if err := publish.SaveUnfinished(dir, d); err != nil {
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
