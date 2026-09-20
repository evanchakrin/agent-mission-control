package desktop

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUnsavedRecordedDirectoryResolution(t *testing.T) {
	f := newFixture(t)
	first := filepath.Join(f.repo, "first")
	second := filepath.Join(f.repo, "second")
	files := []UnsavedPath{{Path: "missing.txt", WorkingDirectory: first}, {Path: "missing.txt", WorkingDirectory: second}, {Path: "missing.txt"}, {Path: "missing.txt", WorkingDirectory: f.home}, {Path: "../missing.txt", WorkingDirectory: first}, {Path: "missing.txt:stream", WorkingDirectory: first}}
	result, err := f.m.CheckUnsavedFiles(context.Background(), files)
	if err != nil || len(result.Files) != len(files) {
		t.Fatal(result, err)
	}
	for i, got := range result.Files {
		if got.RequestedPath != files[i].Path || got.RequestedWorkingDirectory != files[i].WorkingDirectory {
			t.Fatal("lost request context", got)
		}
		if i < 2 {
			if got.State != "missing" || got.Path != filepath.Join(files[i].WorkingDirectory, files[i].Path) {
				t.Fatal("wrong resolved file", got)
			}
		} else if got.State != "unsupported-path" || got.Problem == "" {
			t.Fatal("unsafe context resolved", got)
		}
	}
	for _, file := range []UnsavedPath{{Path: "bad\x00path"}, {Path: "file", WorkingDirectory: "bad\ncontext"}} {
		if _, err := f.m.CheckUnsavedFiles(context.Background(), []UnsavedPath{file}); err == nil {
			t.Fatal("invalid context accepted", file)
		}
	}
	// Do not clean away a traversal component before checking for links.
	if localAbsolutePath(f.repo + string(filepath.Separator) + "linked" + string(filepath.Separator) + ".." + string(filepath.Separator) + "missing.txt") {
		t.Fatal("absolute parent traversal accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	stopped, err := f.m.CheckUnsavedFiles(canceled, files)
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range stopped.Files {
		if got.State != "unknown" || got.RequestedWorkingDirectory != files[i].WorkingDirectory {
			t.Fatal("canceled request lost context", got)
		}
	}
}

func TestUnsavedContextHTTPBinding(t *testing.T) {
	f := newFixture(t)
	f.m.opts.LocalMachineID = "context-machine"
	files := []UnsavedPath{{Path: "missing.txt", WorkingDirectory: f.repo}}
	body := map[string]any{"machineId": "context-machine", "files": files}
	got := mustRequest(t, f.m, "POST", "/api/v2/local/unsaved", body)
	rows := got["files"].([]any)
	row := rows[0].(map[string]any)
	if row["requestedWorkingDirectory"] != f.repo || row["state"] != "missing" {
		t.Fatal(got)
	}
	body["machineId"] = "remote"
	if code, _ := request(t, f.m, "POST", "/api/v2/local/unsaved", body); code != 403 {
		t.Fatal("remote context accepted", code)
	}
	body["machineId"] = "context-machine"
	body["paths"] = []string{f.file}
	if code, _ := request(t, f.m, "POST", "/api/v2/local/unsaved", body); code != 400 {
		t.Fatal("ambiguous request accepted", code)
	}
}
