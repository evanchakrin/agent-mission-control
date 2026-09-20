package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/platform"
)

func TestImportOwnerRejectsRoleAndImplicitSource(t *testing.T) {
	for _, c := range []platform.Config{{Role: platform.Hub}, {Role: platform.Collector}, {Role: platform.Desktop}} {
		if _, err := importOwner(c, ""); err == nil {
			t.Fatal("unsafe maintenance configuration accepted")
		}
	}
}

func TestImportOwnerLockAndResume(t *testing.T) {
	sid, err := platform.CurrentOwnerSID()
	if err != nil {
		t.Skip(err)
	}
	if err := platform.ValidateDesktopIdentity(sid); err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "legacy")
	state := filepath.Join(root, "desktop")
	for _, p := range []string{source, state} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "playbooks.json"), []byte(`{"items":[{"id":"saved","name":"Saved playbook"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := platform.Config{Role: platform.Desktop, DataDir: state, OwnerSID: sid, OwnerHomeDir: root, ListenAddress: "127.0.0.1:4174"}
	lock, err := platform.AcquireInstance(platform.Desktop, state)
	if err != nil {
		t.Fatal(err)
	}
	_, err = importOwner(c, source)
	lock.Close()
	if !errors.Is(err, platform.ErrAlreadyRunning) {
		t.Fatalf("running desktop not protected: %v", err)
	}
	counts, err := importOwner(c, source)
	if err != nil || counts["playbooks"] != 1 {
		t.Fatalf("import: %v %v", counts, err)
	}
	counts, err = importOwner(c, source)
	if err != nil || counts["playbooks"] != 0 {
		t.Fatalf("resume duplicated import: %v %v", counts, err)
	}
}
