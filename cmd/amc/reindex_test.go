package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/indexer"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestReindexOutdatedRetainsKeysAcrossLostAcknowledgement(t *testing.T) {
	var keys []string
	lost := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			page := indexer.VersionPage{Checked: 2, Issues: []indexer.VersionIssue{{SessionID: "preserved-key", SourceID: "source", Generation: "g", RequiredVersion: "4"}, {SourceID: "unmapped", Generation: "g", RequiredVersion: "4"}}}
			if r.URL.Query().Get("cursor") == "" {
				page.Next = "next"
			} else {
				page = indexer.VersionPage{}
			}
			json.NewEncoder(w).Encode(page)
			return
		}
		if r.URL.Path != "/api/v2/sessions/preserved-key/rebuild" {
			t.Error("invented session identity", r.URL.Path)
		}
		var body struct {
			OperationID string `json:"operationId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		keys = append(keys, body.OperationID)
		if lost {
			lost = false
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(store.ProjectionRevision{Session: store.Session{ID: "preserved-key"}, SourceID: "source", Generation: "g", ParserVersion: "4", OperationID: body.OperationID})
	}))
	defer server.Close()
	first, err := queueOutdatedRebuilds(context.Background(), server.Client(), server.URL, "upgrade")
	if err == nil || first.Complete {
		t.Fatal("lost acknowledgement called complete", first, err)
	}
	second, err := queueOutdatedRebuilds(context.Background(), server.Client(), server.URL, "upgrade")
	if err != nil || !second.Complete || second.Queued != 1 || second.Unmapped != 1 || second.Checked != 2 {
		t.Fatal(second, err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatal("retry changed operation key", keys)
	}
}

func TestReindexOutdatedRejectsUnboundedAndNonadvancingPages(t *testing.T) {
	for _, page := range []string{`{"checked":51}`, `{"checked":0,"issues":[{}]}`, `{"checked":1,"next":"stuck"}`} {
		t.Run(page, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(page)) }))
			defer server.Close()
			report, err := queueOutdatedRebuilds(context.Background(), server.Client(), server.URL, "upgrade")
			if err == nil || report.Complete || calls > 2 {
				t.Fatal("invalid page accepted", report, err, calls)
			}
		})
	}
	for _, operation := range []string{"", strings.Repeat("a", 257)} {
		if _, err := queueOutdatedRebuilds(context.Background(), nil, "", operation); err == nil {
			t.Fatal("invalid operation accepted")
		}
	}
}
