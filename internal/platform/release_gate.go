package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var requiredReleaseGates = []string{
	"fleet-table-machines-projects", "accounting-economics-analytics",
	"durable-organization", "backup-restore-migration",
	"durable-ack-crash-boundaries", "organization-ingestion-isolation",
}

// Single-owner release admission: representative real workflows and data safety,
// not synthetic capacity certification. Service/logon startup is checked after
// registration; requiring that evidence here prevented installing it at all.
// Resource measurements and long soaks remain optional diagnostic tools.

func RequiredReleaseGates() []string { return append([]string(nil), requiredReleaseGates...) }

type ReleaseEvidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type ReleaseGate struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	CompletedAt time.Time         `json:"completedAt"`
	Evidence    []ReleaseEvidence `json:"evidence"`
}
type ReleaseGateManifest struct {
	Version          int           `json:"version"`
	ReleaseVersion   string        `json:"releaseVersion"`
	Platform         string        `json:"platform"`
	ExecutableSHA256 string        `json:"executableSHA256"`
	CreatedAt        time.Time     `json:"createdAt"`
	ExpiresAt        time.Time     `json:"expiresAt"`
	Gates            []ReleaseGate `json:"gates"`
}

// VerifyReleaseGate verifies a complete, current evidence manifest bound to the
// exact executable and local evidence bytes. It never manufactures a pass from
// tests, approves deployment or installs anything. The release operator remains
// responsible for truthful evidence and explicit cutover authority. Installation
// additionally requires the manifest/evidence in protected production storage.
func VerifyReleaseGate(manifestPath, executable string) (ReleaseGateManifest, error) {
	return verifyReleaseGateAt(manifestPath, executable, time.Now().UTC())
}
func verifyReleaseGateAt(manifestPath, executable string, now time.Time) (ReleaseGateManifest, error) {
	var result ReleaseGateManifest
	if !filepath.IsAbs(manifestPath) || !filepath.IsAbs(executable) {
		return result, errors.New("release gate paths must be absolute")
	}
	data, err := readGateFile(manifestPath, 1024*1024)
	if err != nil {
		return result, err
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err = d.Decode(&result); err != nil {
		return result, fmt.Errorf("release gate manifest: %w", err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return result, errors.New("release gate manifest must contain exactly one object")
	}
	if result.Version != 1 || !releaseVersion.MatchString(result.ReleaseVersion) || result.ReleaseVersion == "." || result.ReleaseVersion == ".." || result.Platform != "windows/amd64" {
		return result, errors.New("unsupported release gate manifest version, release or platform")
	}
	if result.CreatedAt.IsZero() || result.CreatedAt.After(now.Add(5*time.Minute)) || !result.ExpiresAt.After(now) || result.ExpiresAt.After(result.CreatedAt.Add(30*24*time.Hour)) {
		return result, errors.New("release gate manifest is expired or has invalid validity dates")
	}
	actual, err := gateFileHash(executable, 1<<30)
	if err != nil {
		return result, err
	}
	if !validDigest(result.ExecutableSHA256) || actual != result.ExecutableSHA256 {
		return result, errors.New("release gate executable checksum mismatch")
	}
	if len(result.Gates) != len(requiredReleaseGates) {
		return result, errors.New("release gate manifest does not contain every required acceptance gate")
	}
	required := map[string]bool{}
	for _, id := range requiredReleaseGates {
		required[id] = true
	}
	verified := map[string]string{}
	for _, gate := range result.Gates {
		if !required[gate.ID] {
			return result, fmt.Errorf("duplicate or unknown release gate %q", gate.ID)
		}
		delete(required, gate.ID)
		if gate.Status != "passed" || gate.CompletedAt.IsZero() || gate.CompletedAt.After(result.CreatedAt) || len(gate.Evidence) == 0 || len(gate.Evidence) > 16 {
			return result, fmt.Errorf("release gate %q lacks completed passing evidence", gate.ID)
		}
		for _, evidence := range gate.Evidence {
			path, err := releaseEvidencePath(filepath.Dir(manifestPath), evidence.Path)
			if err != nil {
				return result, err
			}
			if !validDigest(evidence.SHA256) {
				return result, errors.New("invalid release evidence digest")
			}
			if previous, ok := verified[path]; ok {
				if previous != evidence.SHA256 {
					return result, errors.New("conflicting digests for release evidence")
				}
				continue
			}
			if len(verified) >= 128 {
				return result, errors.New("release manifest has too many evidence files")
			}
			actual, err := gateFileHash(path, 64<<20)
			if err != nil {
				return result, err
			}
			if actual != evidence.SHA256 {
				return result, fmt.Errorf("release evidence checksum mismatch for %q", evidence.Path)
			}
			verified[path] = actual
		}
	}
	return result, nil
}
func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}
func releaseEvidencePath(base, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.ContainsAny(relative, "\x00\r\n:") || strings.HasPrefix(relative, `\`) || strings.HasPrefix(relative, "/") {
		return "", errors.New("release evidence must use relative local paths")
	}
	path := filepath.Join(base, filepath.FromSlash(relative))
	if pathKey(path) == pathKey(base) || !within(base, path) {
		return "", errors.New("release evidence escapes its manifest directory")
	}
	canonical, err := safeAbsolutePath(path)
	if err != nil {
		return "", err
	}
	if pathKey(canonical) != pathKey(path) {
		return "", errors.New("release evidence cannot pass through junctions or symlinks")
	}
	return path, nil
}
func readGateFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("release evidence must be a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errors.New("release evidence grew beyond its size limit")
	}
	return b, err
}
func gateFileHash(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return "", errors.New("release artifact must be a bounded regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", errors.New("release artifact grew beyond its size limit")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
