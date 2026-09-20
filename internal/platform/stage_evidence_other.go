//go:build !windows

package platform

func StageReleaseEvidence(string, string) (string, error) { return "", ErrUnsupported }
