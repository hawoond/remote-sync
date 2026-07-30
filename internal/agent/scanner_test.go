package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hawoond/remote-sync/internal/localdb"
)

func TestScannerPlansCreateModifyAndDelete(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	state, err := localdb.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	const folderID = "681f7dd7-559b-4fab-8734-41b00f663425"
	scanner, err := NewScanner(root, folderID, state)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()

	path := filepath.Join(root, "Report.txt")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Planned != 1 || report.Deleted != 0 {
		t.Fatalf("create report = %+v", report)
	}

	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Planned != 1 {
		t.Fatalf("modify report = %+v", report)
	}

	entry, err := state.Entry(context.Background(), folderID, "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	entry.ServerVersion = "d1ad1ad0-7dd9-4da2-aed8-f600c2aa0912"
	if err := state.UpsertEntry(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	report, err = scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Deleted != 1 {
		t.Fatalf("delete report = %+v", report)
	}
}

func TestScannerDoesNotPlanDeleteWhenRootUnavailable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	state, err := localdb.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	scanner, err := NewScanner(root, "681f7dd7-559b-4fab-8734-41b00f663425", state)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(context.Background()); err == nil {
		t.Fatal("expected unavailable root error")
	}
}
