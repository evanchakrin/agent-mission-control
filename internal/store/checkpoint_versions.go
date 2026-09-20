package store

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/evanchakrin/agent-mission-control/internal/protocol"
)

type CheckpointVersion struct {
	SessionID     string
	SourceID      string
	Generation    string
	Version       string
	Valid         bool
	IndexedOffset int64
}

func CheckpointVersionCursor(v CheckpointVersion) string {
	return SourceCursor(SourceState{Source: protocol.Source{SourceID: v.SourceID, Generation: v.Generation}})
}

// SourceCheckpointVersions never transfers parser state or raw metadata to the
// caller. Each result is bounded even when an individual checkpoint is 4 MiB.
// External v1 normalizers have their own checkpoint contract and are excluded.
// Superseded raw generations stay preserved but cannot be rebuilt into the
// current session; auditing them as actionable would leave permanent warnings.
func (s *Store) SourceCheckpointVersions(ctx context.Context, after string, limit int) ([]CheckpointVersion, error) {
	id, gen := "", ""
	if after != "" {
		b, err := base64.RawURLEncoding.DecodeString(after)
		var c []string
		if err != nil || json.Unmarshal(b, &c) != nil || len(c) != 2 || len(c[0]) > 512 || len(c[1]) > 512 {
			return nil, ErrInvalid
		}
		id, gen = c[0], c[1]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source_id,generation,
 CASE WHEN json_valid(parser_state) THEN COALESCE(substr(CAST(json_extract(parser_state,'$.version') AS TEXT),1,256),'') ELSE '' END,
 CASE WHEN json_valid(parser_state) THEN json_type(parser_state)='object' AND COALESCE(json_type(parser_state,'$.version') IN ('text','null'),1) ELSE 0 END,
 indexed_offset,COALESCE((SELECT id FROM sessions x WHERE x.source_id=sources.source_id AND x.generation=sources.generation ORDER BY id LIMIT 1),'') FROM sources
 WHERE (source_id>? OR (source_id=? AND generation>?))
 AND EXISTS(SELECT 1 FROM source_identity i WHERE i.source_id=sources.source_id AND i.active_generation=sources.generation)
 AND NOT EXISTS(SELECT 1 FROM properties p WHERE p.key='external_index:'||sources.source_id AND p.value='true')
 ORDER BY source_id,generation LIMIT ?`, id, id, gen, pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []CheckpointVersion{}
	for rows.Next() {
		var v CheckpointVersion
		if err := rows.Scan(&v.SourceID, &v.Generation, &v.Version, &v.Valid, &v.IndexedOffset, &v.SessionID); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
