// Command check-compact-ledger opens a disposable stats-only ledger with the
// production Go store and validates that the dashboard's aggregate path works.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func main() {
	dir := flag.String("dir", "", "absolute compact hub data directory")
	flag.Parse()
	if !filepath.IsAbs(*dir) {
		fail("--dir must be absolute")
	}
	s, err := store.Open(*dir, store.Options{})
	if err != nil {
		fail(err.Error())
	}
	defer s.Close()
	if !s.StatsOnly() {
		fail("ledger is not stats-only")
	}
	ctx := context.Background()
	if err = s.SetupAnalytics(ctx); err != nil {
		fail(err.Error())
	}
	if err = s.SetupIndexing(ctx); err != nil {
		fail(err.Error())
	}
	if err = s.IntegrityCheck(ctx); err != nil {
		fail(err.Error())
	}
	totals, err := s.SessionTotals(ctx, store.SessionQuery{})
	if err != nil {
		fail(err.Error())
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"statsOnly": true, "sessions": totals.Sessions,
		"events": totals.Events, "tokensIn": totals.TokensIn, "tokensCache": totals.TokensCache,
		"tokensCacheWrite": totals.TokensCacheWrite, "tokensOut": totals.TokensOut})
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
