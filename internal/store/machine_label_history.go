package store

import (
	"context"
	"encoding/json"
)

type MachineLabelAudit struct {
	OperationID string       `json:"operationId"`
	Revision    int64        `json:"revision"`
	Before      MachineLabel `json:"before"`
	After       MachineLabel `json:"after"`
	At          string       `json:"at"`
}

type MachineLabelAuditPage struct {
	Items []MachineLabelAudit `json:"items"`
	Next  int64               `json:"next,omitempty"`
}

func (s *Store) MachineLabelHistory(ctx context.Context, id string, after int64, limit int) (MachineLabelAuditPage, error) {
	page := MachineLabelAuditPage{Items: []MachineLabelAudit{}}
	if !validID(id) || after < 0 {
		return page, ErrInvalid
	}
	limit = pageLimit(limit)
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id,revision,before_json,after_json,at FROM machine_label_audit WHERE machine_id=? AND revision>? ORDER BY revision LIMIT ?`, id, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var item MachineLabelAudit
		var before, current []byte
		if err = rows.Scan(&item.OperationID, &item.Revision, &before, &current, &item.At); err != nil {
			return page, err
		}
		if err = json.Unmarshal(before, &item.Before); err != nil {
			return page, err
		}
		if err = json.Unmarshal(current, &item.After); err != nil {
			return page, err
		}
		page.Items = append(page.Items, item)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.Next = page.Items[limit-1].Revision
	}
	return page, nil
}
