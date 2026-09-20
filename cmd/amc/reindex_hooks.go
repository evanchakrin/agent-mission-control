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
	"strings"
	"time"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

type hookQueueJob struct {
	SessionID string `json:"sessionId"`
	Revision  string `json:"revision"`
	State     string `json:"state"`
}
type hookQueueReport struct {
	Checked          int            `json:"checked"`
	Skipped          int            `json:"alreadyCoveredOrUnsupported"`
	Jobs             []hookQueueJob `json:"queuedOrPreviouslyAccepted"`
	Complete         bool           `json:"queuePassComplete"`
	LimitReached     bool           `json:"queueLimitReached"`
	AttemptedSession string         `json:"lastAttemptedSession,omitempty"`
}

func hookOwnerJSON(ctx context.Context, client *http.Client, base, method, path string, body, result any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("hook recovery owner request returned HTTP %d; retain operation ID before retrying", res.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("hook recovery response exceeds 1 MiB")
	}
	return json.Unmarshal(data, result)
}

// This queues a bounded number, not an unbounded background migration. Publication
// remains the hub's verified staged operation. Repeat the same operation after
// interruption; after accepted jobs finish, covered sources are skipped.
func queueHookRebuilds(ctx context.Context, client *http.Client, base, operation string, maxJobs int) (report hookQueueReport, err error) {
	return queueEvidenceRebuilds(ctx, client, base, operation, maxJobs, "javascript")
}

func queueEvidenceRebuilds(ctx context.Context, client *http.Client, base, operation string, maxJobs int, kind string) (report hookQueueReport, err error) {
	report.Jobs = []hookQueueJob{}
	filter, namespace := "missingJavaScriptEvidence", "hook-evidence-v1"
	switch kind {
	case "javascript":
	case "undo":
		filter, namespace = "missingUndoEvidence", "undo-evidence-v1"
	default:
		return report, errors.New("evidence must be javascript or undo")
	}
	if strings.TrimSpace(operation) == "" || len(operation) > 256 || maxJobs < 1 || maxJobs > 25 {
		return report, errors.New("hook recovery requires an operation ID (1-256 bytes) and max-rebuilds between 1 and 25")
	}
	cursor := ""
	for {
		var page store.SessionPage
		if err = hookOwnerJSON(ctx, client, base, http.MethodGet, "/api/v2/sessions?limit=50&"+filter+"=true&cursor="+url.QueryEscape(cursor), nil, &page); err != nil {
			return
		}
		if page.Sessions == nil || len(page.Sessions) > 50 || page.NextCursor != "" && (page.NextCursor == cursor || len(page.Sessions) == 0) {
			return report, errors.New("invalid hook recovery catalog page")
		}
		for _, row := range page.Sessions {
			report.Checked++
			if row.ID == "" || row.SourceID == "" || row.Generation == "" {
				return report, errors.New("hook recovery session identity missing")
			}
			if (row.Provider != "claude" && row.Provider != "codex") || !strings.HasPrefix(row.Completeness, "indexed-source") {
				report.Skipped++
				continue
			}
			path := "/api/v2/sessions/" + url.PathEscape(row.ID)
			state := ""
			if kind == "undo" {
				var evidence store.GitUndoPage
				if err = hookOwnerJSON(ctx, client, base, http.MethodGet, path+"/git-undos?limit=1", nil, &evidence); err != nil {
					return
				}
				if len(evidence.Snapshot) != 64 {
					return report, errors.New("undo evidence snapshot missing")
				}
				state = evidence.State
			} else {
				var evidence store.JavaScriptHookEvidence
				if err = hookOwnerJSON(ctx, client, base, http.MethodGet, path+"/hook-evidence/javascript", nil, &evidence); err != nil {
					return
				}
				if evidence.SessionID != row.ID {
					return report, errors.New("hook evidence session identity changed")
				}
				state = evidence.State
			}
			switch state {
			case "indexed-history", "incomplete-indexing":
				report.Skipped++
				continue
			case "needs-rebuild":
			default:
				return report, errors.New("unknown hook evidence state")
			}
			key := parser.ID(namespace, operation, row.ID, row.SourceID, row.Generation, parser.Version)
			report.AttemptedSession = row.ID
			var job store.ProjectionRevision
			if err = hookOwnerJSON(ctx, client, base, http.MethodPost, path+"/rebuild", map[string]string{"operationId": key}, &job); err != nil {
				return
			}
			if job.Session.ID != row.ID || job.SourceID != row.SourceID || job.Generation != row.Generation || job.OperationID != key || job.ParserVersion != parser.Version || job.Revision == "" {
				return report, errors.New("rebuild receipt identity changed; inspect accepted work before retrying")
			}
			switch job.State {
			case "building", "verifying", "ready", "active", "retired":
			default:
				return report, errors.New("unknown rebuild receipt state; inspect accepted work before retrying")
			}
			report.Jobs = append(report.Jobs, hookQueueJob{row.ID, job.Revision, job.State})
			if len(report.Jobs) == maxJobs {
				report.LimitReached = true
				return report, nil
			}
		}
		if page.NextCursor == "" {
			report.Complete = true
			return report, nil
		}
		cursor = page.NextCursor
	}
}
