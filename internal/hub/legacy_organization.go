package hub

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/evanchakrin/agent-mission-control/internal/store"
)

// Only the named-pipe owner mux registers this explicit migration operation.
// Callers supply metadata, never file paths or authority to remap identities.
func (h *Hub) legacyOrganizationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v2/migrations/legacy-organization", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Sessions map[string]json.RawMessage `json:"sessions"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			fail(w, store.ErrInvalid)
			return
		}
		if decoder.Decode(new(any)) != io.EOF || len(request.Sessions) == 0 || len(request.Sessions) > 1000 {
			fail(w, store.ErrInvalid)
			return
		}
		for _, raw := range request.Sessions {
			var object map[string]json.RawMessage
			if json.Unmarshal(raw, &object) != nil || object == nil {
				fail(w, store.ErrInvalid)
				return
			}
		}
		// Alias resolution occurs under the import writer transaction. Existing
		// owner metadata and already-established source identities take precedence.
		if err := h.Store.ImportLegacyMetadata(r.Context(), nil, request.Sessions); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "processed": len(request.Sessions), "existingOwnerStatePreserved": true})
	})
}
