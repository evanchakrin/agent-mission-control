package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestAsyncProjectDeletionOwnerOnlyAndReceipt(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	_, err = s.MutateProject(ctx, store.ProjectMutation{ID: "p", Name: "Project", Color: "#123456", OperationID: "create"})
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, testToken, "test")
	body := `{"id":"p","revision":1,"delete":true,"operationId":"remove"}`
	request := func(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Prefer", "respond-async")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, item := range []struct{ method, path, body string }{{"POST", "/api/v2/projects", body}, {"GET", "/api/v2/project-deletions/remove", ""}} {
		if w := request(h.IngestionHandler(), item.method, item.path, item.body); w.Code != 404 {
			t.Fatal("network owner access", w.Code)
		}
	}
	owner := h.OwnerHandler()
	for i := 0; i < 2; i++ {
		w := request(owner, "POST", "/api/v2/projects", body)
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
		var job store.ProjectDeletion
		if err = json.Unmarshal(w.Body.Bytes(), &job); err != nil || job.State != "pending" || job.OperationID != "remove" {
			t.Fatal(job, err)
		}
	}
	if _, err = s.AdvanceProjectDeletion(ctx, "remove", 100); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ method, path, body string }{{"POST", "/api/v2/projects", body}, {"GET", "/api/v2/project-deletions/remove", ""}} {
		w := request(owner, item.method, item.path, item.body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var job store.ProjectDeletion
		if err = json.Unmarshal(w.Body.Bytes(), &job); err != nil || job.State != "complete" || !job.Project.Deleted {
			t.Fatal(job, err)
		}
	}
}
