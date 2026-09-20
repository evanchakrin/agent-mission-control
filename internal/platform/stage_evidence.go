package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type evidenceStageFiles struct {
	directory func(string) error
	file      func(string) (*os.File, error)
}

// Stage only already-verified evidence. Test file operations are isolated from
// the production Windows ACL implementation; no acceptance is inferred here.
func stageReleaseEvidence(manifestPath, executable string, files evidenceStageFiles) (string, error) {
	manifest, err := VerifyReleaseGate(manifestPath, executable)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(filepath.Dir(executable), "acceptance")
	if err = files.directory(destination); err != nil {
		return "", err
	}
	stagedManifest := filepath.Join(destination, "release-manifest.json")
	created := map[string]bool{pathKey(destination): true}
	copied := map[string]bool{}
	for _, gate := range manifest.Gates {
		for _, evidence := range gate.Evidence {
			source, err := releaseEvidencePath(filepath.Dir(manifestPath), evidence.Path)
			if err != nil {
				return "", err
			}
			target, err := releaseEvidencePath(destination, evidence.Path)
			if err != nil {
				return "", err
			}
			if pathKey(target) == pathKey(stagedManifest) {
				return "", errors.New("evidence path conflicts with staged manifest")
			}
			if copied[pathKey(target)] {
				continue
			}
			// Verify the exact bytes before publishing them, not just the earlier read.
			data, err := readGateFile(source, 64<<20)
			if err != nil {
				return "", err
			}
			hash := sha256.Sum256(data)
			if hex.EncodeToString(hash[:]) != evidence.SHA256 {
				return "", errors.New("release evidence changed during staging")
			}
			var parents []string
			for p := filepath.Dir(target); !created[pathKey(p)]; p = filepath.Dir(p) {
				if !within(destination, p) {
					return "", errors.New("evidence directory escaped staging root")
				}
				parents = append(parents, p)
			}
			for i := len(parents) - 1; i >= 0; i-- {
				if err = files.directory(parents[i]); err != nil {
					return "", err
				}
				created[pathKey(parents[i])] = true
			}
			if err = writeStagedEvidence(target, data, files); err != nil {
				return "", err
			}
			copied[pathKey(target)] = true
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	if err = writeStagedEvidence(stagedManifest, data, files); err != nil {
		return "", err
	}
	if _, err = VerifyReleaseGate(stagedManifest, executable); err != nil {
		return "", err
	}
	return stagedManifest, nil
}
func writeStagedEvidence(path string, data []byte, files evidenceStageFiles) error {
	f, err := files.file(path)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	return closed
}
