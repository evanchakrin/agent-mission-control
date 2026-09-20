package store

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func legacyAssetName(name string) bool {
	if name == "" || len(name) > 4096 || strings.ContainsAny(name, "\\:\x00") || path.Clean(name) != name || strings.HasPrefix(name, "/") {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if strings.TrimRight(component, " .") != component {
			return false
		}
		base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return false
		}
	}
	return name == "migration-manifest.json" || name == "legacy-assets" || strings.HasPrefix(name, "legacy-assets/")
}

// Legacy migration evidence is immutable auxiliary history, not transcript
// chunks. Stream it into a separately checksummed archive; never accumulate a
// filename manifest or complete asset bodies in memory.
func backupLegacyAssets(ctx context.Context, source, dest string) (string, error) {
	archive, err := os.OpenFile(filepath.Join(dest, "legacy-assets.tar"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	tw := tar.NewWriter(archive)
	for _, root := range []string{"legacy-assets", "migration-manifest.json"} {
		base := filepath.Join(source, root)
		if _, err = os.Lstat(base); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return "", err
		}
		err = walkLegacyAssetDirectory(ctx, base, 0, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return fmt.Errorf("legacy backup refuses non-regular asset: %s", p)
			}
			rel, err := filepath.Rel(source, p)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(rel)
			if !legacyAssetName(name) {
				return fmt.Errorf("unsafe legacy asset name")
			}
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = name
			if err = tw.WriteHeader(header); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			n, err := io.Copy(tw, &contextReader{ctx: ctx, r: in})
			if err != nil {
				return err
			}
			after, err := in.Stat()
			if err != nil {
				return err
			}
			if n != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
				return fmt.Errorf("legacy asset changed during backup")
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	if err = tw.Close(); err != nil {
		return "", err
	}
	if err = archive.Sync(); err != nil {
		return "", err
	}
	if err = archive.Close(); err != nil {
		return "", err
	}
	return fileHash(ctx, filepath.Join(dest, "legacy-assets.tar"))
}

// WalkDir sorts by loading every name in one directory. Imported archives can
// contain very wide directories, so enumerate fixed batches instead. Deep
// unexpected trees fail visibly instead of exhausting handles or stack space.
func walkLegacyAssetDirectory(ctx context.Context, p string, depth int, visit fs.WalkDirFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return fmt.Errorf("legacy asset directory nesting exceeds safe backup traversal depth")
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if err = visit(p, fs.FileInfoToDirEntry(info), nil); err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	dir, err := os.Open(p)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, readErr := dir.ReadDir(128)
		for _, entry := range entries {
			if err = walkLegacyAssetDirectory(ctx, filepath.Join(p, entry.Name()), depth+1, visit); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// Empty destination validates only. Restore accepts no links, absolute paths,
// alternate data streams, path traversal, devices or overwrites.
func readLegacyAssets(ctx context.Context, backup, dest string) error {
	in, err := os.Open(filepath.Join(backup, "legacy-assets.tar"))
	if err != nil {
		return err
	}
	defer in.Close()
	tr := tar.NewReader(&contextReader{ctx: ctx, r: in})
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if !legacyAssetName(header.Name) || header.Size < 0 || header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsafe legacy backup entry")
		}
		if dest == "" {
			if _, err = io.Copy(io.Discard, tr); err != nil {
				return err
			}
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(header.Name))
		if header.Typeflag == tar.TypeDir {
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if err == nil {
			err = out.Sync()
		}
		closed := out.Close()
		if err != nil {
			return err
		}
		if closed != nil {
			return closed
		}
	}
}
