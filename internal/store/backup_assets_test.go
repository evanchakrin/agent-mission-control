package store

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyAssetTraversalVisitsWideDirectoryInBoundedBatches(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 513; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("asset-%d", i)), []byte("evidence"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	if err := walkLegacyAssetDirectory(context.Background(), root, 0, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 513 {
		t.Fatal("batch traversal dropped files", count)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := walkLegacyAssetDirectory(ctx, root, 0, func(string, fs.DirEntry, error) error { t.Fatal("cancelled walker visited data"); return nil }); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestBackupRestoresLegacyEvidenceAndDetectsCorruption(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "hub"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assets := filepath.Join(s.dir, "legacy-assets")
	if err = os.MkdirAll(assets, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"econ-history.jsonl": "{\"at\":123,\"totalUsd\":42}\nmalformed retained\n", "empty": ""} {
		if err = os.WriteFile(filepath.Join(assets, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(s.dir, "migration-manifest.json"), []byte(`{"preserve":"source evidence"}`), 0600); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(root, "backup")
	manifest, err := s.Backup(context.Background(), backup)
	if err != nil || manifest.Version != 2 || manifest.LegacyAssetsSHA256 == "" {
		t.Fatal(manifest, err)
	}
	restored, err := RestoreBackup(context.Background(), backup, filepath.Join(root, "restored"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	originalManifest, err := os.ReadFile(filepath.Join(backup, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	missing := manifest
	missing.LegacyAssetsSHA256 = ""
	raw, _ := json.Marshal(missing)
	if err = os.WriteFile(filepath.Join(backup, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyBackup(context.Background(), backup); err == nil {
		t.Fatal("format 2 accepted omitted asset checksum")
	}
	if err = os.WriteFile(filepath.Join(backup, "manifest.json"), originalManifest, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"legacy-assets/econ-history.jsonl", "legacy-assets/empty", "migration-manifest.json"} {
		original, e := os.ReadFile(filepath.Join(s.dir, name))
		if e != nil {
			t.Fatal(e)
		}
		actual, e := os.ReadFile(filepath.Join(restored.dir, name))
		if e != nil || !bytes.Equal(original, actual) {
			t.Fatal("lost legacy evidence", name, e)
		}
	}
	f, err := os.OpenFile(filepath.Join(backup, "legacy-assets.tar"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write([]byte("corruption"))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyBackup(context.Background(), backup); err == nil {
		t.Fatal("corrupted asset archive verified")
	}
}

func TestLegacyArchiveRejectsUnsafeNamesAndLinks(t *testing.T) {
	for _, name := range []string{"../escape", "legacy-assets/../../escape", "/absolute", "legacy-assets/x:stream", "legacy-assets/NUL.txt", "legacy-assets/trailing.", "ledger.sqlite", "legacy-assets\\escape"} {
		if legacyAssetName(name) {
			t.Errorf("unsafe name accepted: %s", name)
		}
	}
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeChar} {
		root := t.TempDir()
		var data bytes.Buffer
		tw := tar.NewWriter(&data)
		if err := tw.WriteHeader(&tar.Header{Name: "legacy-assets/link", Typeflag: kind, Linkname: "../outside"}); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		if err := os.WriteFile(filepath.Join(root, "legacy-assets.tar"), data.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if err := readLegacyAssets(context.Background(), root, ""); err == nil {
			t.Fatal("unsafe archive verified", kind)
		}
	}
}
