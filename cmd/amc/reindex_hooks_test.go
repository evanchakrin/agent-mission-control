package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/parser"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestHookRecoveryBoundsQueueAndRetainsLostAckIdentity(t *testing.T) {
	lost, covered := true, false
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/sessions":
			rows := []store.Session{}
			for _, id := range []string{"one", "two"} {
				rows = append(rows, store.Session{ID: id, SourceID: "source-" + id, Generation: "g", Provider: "claude", Completeness: "indexed-source"})
			}
			json.NewEncoder(w).Encode(store.SessionPage{Sessions: rows})
		default:
			id := strings.Split(r.URL.Path, "/")[4]
			if r.Method == http.MethodGet {
				state := "needs-rebuild"
				if covered {
					state = "indexed-history"
				}
				json.NewEncoder(w).Encode(store.JavaScriptHookEvidence{SessionID: id, State: state})
				return
			}
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			keys = append(keys, body["operationId"])
			if lost {
				lost = false
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(store.ProjectionRevision{Revision: "revision-" + id, State: "building", Session: store.Session{ID: id}, SourceID: "source-" + id, Generation: "g", ParserVersion: parser.Version, OperationID: body["operationId"]})
		}
	}))
	defer server.Close()
	first, err := queueHookRebuilds(context.Background(), server.Client(), server.URL, "recover", 1)
	if err == nil || first.Complete || first.AttemptedSession != "one" || len(first.Jobs) != 0 {
		t.Fatal(first, err)
	}
	second, err := queueHookRebuilds(context.Background(), server.Client(), server.URL, "recover", 1)
	if err != nil || second.Complete || !second.LimitReached || len(second.Jobs) != 1 || second.Checked != 1 {
		t.Fatal(second, err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatal(keys)
	}
	covered = true
	last, err := queueHookRebuilds(context.Background(), server.Client(), server.URL, "recover", 1)
	if err != nil || !last.Complete || last.Skipped != 2 || len(last.Jobs) != 0 || len(keys) != 2 {
		t.Fatal(last, err)
	}
}

func TestHookRecoveryRejectsInvalidInputsWithoutRequests(t *testing.T) {
	if _, err := queueEvidenceRebuilds(context.Background(), nil, "", "run", 1, "unknown"); err == nil {
		t.Fatal("invalid evidence accepted")
	}
	for _, maximum := range []int{0, 26} {
		if _, err := queueHookRebuilds(context.Background(), nil, "", "run", maximum); err == nil {
			t.Fatal(maximum)
		}
	}
	for _, operation := range []string{"", " ", strings.Repeat("x", 257)} {
		if _, err := queueHookRebuilds(context.Background(), nil, "", operation, 1); err == nil {
			t.Fatal(operation)
		}
	}
}

func TestHookRecoveryRejectsMalformedCatalogBeforeWrites(t *testing.T) {
	for _, body := range []string{`{}`, `{"sessions":[{}]}`, `{"sessions":[],"nextCursor":"stuck"}`, strings.Repeat("x", (1<<20)+1)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
				}
				w.Write([]byte(body))
			}))
			defer server.Close()
			got, err := queueHookRebuilds(context.Background(), server.Client(), server.URL, "run", 1)
			if err == nil || got.Complete || writes != 0 {
				t.Fatal(got, err, writes)
			}
		})
	}
}
