package desktop

import (
	"bufio"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ImportLegacy is an explicit local migration operation. It reads fixed files
// under the supplied legacy state directory and refuses to overwrite owner
// state that has already been initialized. Its result is reviewable by section.
func (m *Manager) ImportLegacy(directory string) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !filepath.IsAbs(directory) || strings.HasPrefix(directory, `\\`) {
		return nil, errors.New("legacy owner state requires an absolute local directory")
	}
	if err := noLinks(directory); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, section := range []struct{ file, key, field string }{{"playbooks.json", "playbooks", "items"}, {"directives.json", "directives", "items"}, {"directive-roots.json", "roots", "roots"}, {"triage.json", "triage", ""}} {
		p := filepath.Join(directory, section.file)
		if err = noLinks(p); err != nil {
			return nil, err
		}
		f, e := os.Open(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		data, e := io.ReadAll(io.LimitReader(f, 16*1024*1024+1))
		f.Close()
		if e != nil {
			return nil, e
		}
		if len(data) > 16*1024*1024 {
			return nil, errors.New("legacy owner metadata is larger than the migration limit")
		}
		var value any
		if e = json.Unmarshal(data, &value); e != nil {
			return nil, e
		}
		if section.field != "" {
			obj, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("invalid legacy metadata object")
			}
			value = obj[section.field]
			if value == nil {
				value = []any{}
			}
		}
		b, e := json.Marshal(value)
		if e != nil {
			return nil, e
		}
		var exists int
		if err = tx.QueryRow("SELECT count(*) FROM local_state WHERE key=?", section.key).Scan(&exists); err != nil {
			return nil, err
		}
		importKey := "metadata:" + norm(p)
		already, e := importedRecord(tx, importKey, hash(b))
		if e != nil {
			return nil, e
		}
		if already {
			continue
		}
		if exists > 0 {
			return nil, fmt.Errorf("%s owner state is already initialized", section.key)
		}
		if _, e = tx.Exec("INSERT INTO local_state(key,value)VALUES(?,?)", section.key, string(b)); e != nil {
			return nil, e
		}
		if _, e = tx.Exec("INSERT INTO legacy_imports(source,digest,imported_at)VALUES(?,?,?)", importKey, hash(b), now()); e != nil {
			return nil, e
		}
		switch v := value.(type) {
		case []any:
			counts[section.key] = len(v)
		case map[string]any:
			counts[section.key] = len(v)
		}
	}
	if _, err = tx.Exec("INSERT INTO audit(at,kind,path,status,detail)VALUES(?,?,?,?,?)", now(), "legacy-owner-import", directory, "applied", "{}"); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if err = m.importAudit(directory, counts); err != nil {
		return counts, err
	}
	// Only snapshot folders corresponding to validated local inventory targets
	// are imported. A hash directory from a remote path cannot confer authority.
	items, err := m.inventory()
	if err != nil {
		return counts, err
	}
	for _, item := range items {
		h := sha1.Sum([]byte(item.Path))
		key := hex.EncodeToString(h[:])
		folder := filepath.Join(directory, "brain-history", key)
		if noLinks(folder) != nil {
			continue
		}
		files, e := os.ReadDir(folder)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return counts, e
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".snap") {
				continue
			}
			p := filepath.Join(folder, file.Name())
			if noLinks(p) != nil {
				continue
			}
			content, e := readBounded(p)
			if e != nil {
				return counts, e
			}
			stamp := "legacy-" + key + "-" + strings.TrimSuffix(file.Name(), ".snap")
			res, e := m.db.Exec("INSERT OR IGNORE INTO snapshots(stamp,path,content,hash,mtime,created_at)VALUES(?,?,?,?,?,?)", stamp, item.Path, content, hash(content), 0, now())
			if e != nil {
				return counts, e
			}
			n, _ := res.RowsAffected()
			counts["snapshots"] += int(n)
		}
	}
	return counts, nil
}

func importedRecord(tx *sql.Tx, key, digest string) (bool, error) {
	var previous string
	err := tx.QueryRow("SELECT digest FROM legacy_imports WHERE source=?", key).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if previous != digest {
		return false, errors.New("legacy input changed after part of it was imported; preserve the original source and retry")
	}
	return true, nil
}

// Each source line is independently checkpointed with its exact digest. A
// interrupted or malformed-tail import can resume without duplicating history;
// changed already-imported lines are never silently accepted.
func (m *Manager) importAudit(directory string, counts map[string]int) error {
	path := filepath.Join(directory, "audit.jsonl")
	if err := noLinks(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		b := scanner.Bytes()
		if strings.TrimSpace(string(b)) == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal(b, &entry) != nil {
			return fmt.Errorf("legacy audit line %d is malformed; prior imported rows were retained", line)
		}
		key := fmt.Sprintf("audit:%s:%d", norm(path), line)
		tx, err := m.db.Begin()
		if err != nil {
			return err
		}
		already, err := importedRecord(tx, key, hash(b))
		if err != nil {
			tx.Rollback()
			return err
		}
		if already {
			tx.Rollback()
			continue
		}
		at := now()
		if v, ok := entry["at"].(float64); ok && v >= 0 {
			at = int64(v)
		}
		kind, _ := entry["kind"].(string)
		if kind == "" {
			kind = "legacy"
		}
		entryPath, _ := entry["path"].(string)
		status, _ := entry["status"].(string)
		if status == "" {
			status = "recorded"
		}
		_, err = tx.Exec("INSERT INTO audit(at,kind,path,status,detail)VALUES(?,?,?,?,?)", at, kind, entryPath, status, string(b))
		if err == nil {
			_, err = tx.Exec("INSERT INTO legacy_imports(source,digest,imported_at)VALUES(?,?,?)", key, hash(b), now())
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		if err != nil {
			return err
		}
		counts["audit"]++
	}
	return scanner.Err()
}
