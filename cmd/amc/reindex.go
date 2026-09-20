package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type reindexReport struct {
	Checked  int  `json:"checked"`
	Queued   int  `json:"queuedOrPreviouslyAccepted"`
	Unmapped int  `json:"withoutSession"`
	Complete bool `json:"queuePassComplete"`
}

// Stream bounded pages through the owner pipe. Reusing the operation ID after
// interruption resumes safely: each source/generation has a deterministic key.
// Queuing is not rebuilding; no checkpoints or projections are reset here.
func queueOutdatedRebuilds(ctx context.Context, client *http.Client, base, operation string) (report reindexReport, err error) {
	if operation == "" || len(operation) > 256 {
		return report, errors.New("reindex-outdated requires --operation-id (1-256 bytes); reuse it after interruption")
	}
	request := func(method, path string, body any, result any) error {
		var data []byte
		if body != nil {
			var e error
			data, e = json.Marshal(body)
			if e != nil {
				return e
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		req, e := http.NewRequestWithContext(callCtx, method, base+path, bytes.NewReader(data))
		if e != nil {
			return e
		}
		req.Header.Set("Content-Type", "application/json")
		res, e := client.Do(req)
		if e != nil {
			return e
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return fmt.Errorf("reindex owner request returned HTTP %d; retain operation ID and retry after resolving the problem", res.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(result)
	}
	cursor := ""
	for {
		var page indexer.VersionPage
		if err = request(http.MethodGet, "/api/v2/indexing/versions?limit=50&cursor="+url.QueryEscape(cursor), nil, &page); err != nil {
			return
		}
		if page.Checked < 0 || page.Checked > 50 || len(page.Issues) > page.Checked || (page.Next != "" && page.Next == cursor) {
			return report, errors.New("invalid version audit page")
		}
		report.Checked += page.Checked
		for _, issue := range page.Issues {
			if issue.SessionID == "" {
				report.Unmapped++
				continue
			}
			var job store.ProjectionRevision
			key := parser.ID("reindex-outdated", operation, issue.SessionID, issue.SourceID, issue.Generation, issue.RequiredVersion)
			if err = request(http.MethodPost, "/api/v2/sessions/"+url.PathEscape(issue.SessionID)+"/rebuild", map[string]string{"operationId": key}, &job); err != nil {
				return
			}
			if job.Session.ID != issue.SessionID || job.SourceID != issue.SourceID || job.Generation != issue.Generation || job.ParserVersion != issue.RequiredVersion || job.OperationID != key {
				return report, errors.New("source changed while queueing; inspect accepted rebuild before retrying")
			}
			report.Queued++
		}
		if page.Next == "" {
			report.Complete = true
			return
		}
		cursor = page.Next
	}
}
