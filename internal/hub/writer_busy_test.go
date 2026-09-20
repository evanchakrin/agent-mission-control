package hub

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
	"github.com/evanchakrin/agent-mission-control/internal/store"
)

func TestWriterQueueOverflowIsRetryableBeforeAcknowledgement(t *testing.T) {
	w := httptest.NewRecorder()
	fail(w, fmt.Errorf("wrapped: %w", store.ErrWriterQueueFull))
	var response protocol.APIError
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 503 || response.Code != "writer_busy" || !response.Retryable || w.Header().Get("Retry-After") == "" {
		t.Fatal(w.Code, response, w.Header())
	}
}
