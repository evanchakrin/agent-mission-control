package query

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestIndexedExportDownloadContract(t *testing.T) {
	_, mux := queryFixture(t)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/sessions/session/export.ndjson", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Disposition") == "" {
		t.Fatal(w.Code, w.Header())
	}
	scanner := bufio.NewScanner(w.Body)
	var first, last map[string]any
	count := 0
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			first = record
		}
		last = record
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 6 || first["type"] != "header" || last["type"] != "complete" || first["boundary"] != last["boundary"] {
		t.Fatal(count, first, last)
	}
	response(t, mux, "/api/v2/sessions/missing/export.ndjson", 404)
}

func TestGoIndexedExportAcceptedByOfflineReader(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for cross-language replay verification")
	}
	_, mux := queryFixture(t)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/sessions/session/export.ndjson", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	reader, err := filepath.Abs("../../public/v2-replay.js")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// The entire input is a tiny disposable query fixture, not production history.
	// The reader itself still consumes the Blob through its bounded slice API.
	program := `const {validate,page}=require(process.argv[1]);const chunks=[];process.stdin.on('data',x=>chunks.push(x));process.stdin.on('end',async()=>{try{const file=new Blob(chunks);const result=await validate(file);let offset=0,records=0;do{const p=await page(file,{offset,limit:1});records+=p.rows.length;offset=p.nextOffset;}while(offset!==null);if(records!==result.records)throw Error('page traversal lost records');console.log(JSON.stringify({id:result.header.session.id,records,events:result.events,observations:result.observations}));}catch(error){console.error(error.message);process.exitCode=1;}});`
	cmd := exec.CommandContext(ctx, node, "-e", program, reader)
	cmd.Stdin = bytes.NewReader(w.Body.Bytes())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go export / JS reader mismatch: %v %s", err, output)
	}
	var summary struct {
		ID                            string `json:"id"`
		Records, Events, Observations int
	}
	if err := json.Unmarshal(output, &summary); err != nil {
		t.Fatal(err, string(output))
	}
	if summary.ID != "session" || summary.Records != 6 || summary.Events+summary.Observations != 4 {
		t.Fatalf("incomplete cross-language replay: %+v", summary)
	}
}
