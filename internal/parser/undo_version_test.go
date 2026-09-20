package parser

import (
	"errors"
	"testing"
)

func TestPreArgvUndoCheckpointRequiresRebuild(t *testing.T) {
	if _, err := DecodeState([]byte(`{"version":"5","nativeId":"retained-session"}`)); !errors.Is(err, ErrRebuildRequired) {
		t.Fatal("pre-edit-context checkpoint accepted", err)
	}
	if _, err := DecodeState([]byte(`{"version":"4","nativeId":"retained-session"}`)); !errors.Is(err, ErrRebuildRequired) {
		t.Fatalf("pre-argv checkpoint must require staged rebuild: %v", err)
	}
	state, err := DecodeState(nil)
	if err != nil || state.Version != Version || Version == "4" {
		t.Fatal(state.Version, err)
	}
}
