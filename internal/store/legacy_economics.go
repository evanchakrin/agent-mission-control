package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

type LegacyEconomicsEntry struct {
	Offset      int64  `json:"offset"`
	Length      int    `json:"length"`
	Complete    bool   `json:"complete"`
	ValidObject bool   `json:"validObject"`
	Raw         string `json:"raw"`
}
type LegacyEconomicsPage struct {
	Evidence   string                 `json:"evidence"`
	Items      []LegacyEconomicsEntry `json:"items"`
	NextCursor string                 `json:"nextCursor,omitempty"`
}

func (s *Store) OpenLegacyEconomics() (*os.File, error) {
	f, err := os.OpenInRoot(s.dir, "legacy-assets/econ-history.jsonl")
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, ErrInvalid
	}
	return f, nil
}

// Legacy rows are source evidence, never converted into verified measurements.
// Byte cursors retain duplicate/malformed/partial lines; the raw download is
// authoritative for invalid UTF-8 or records exceeding the display bound.
func (s *Store) LegacyEconomics(ctx context.Context, cursor string, limit int) (LegacyEconomicsPage, error) {
	page := LegacyEconomicsPage{Evidence: "legacy-unverified", Items: []LegacyEconomicsEntry{}}
	f, err := s.OpenLegacyEconomics()
	if errors.Is(err, ErrNotFound) {
		if cursor != "" {
			return page, ErrHistoryChanged
		}
		return page, nil
	}
	if err != nil {
		return page, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return page, err
	}
	identity := fmt.Sprintf("%d:%d:%s", info.Size(), info.ModTime().UnixNano(), s.RecoveryEpoch())
	after, has, err := decodeKey(cursor, "legacy-economics")
	if err != nil {
		return page, err
	}
	var offset int64
	if has {
		parts := strings.SplitN(after, "|", 2)
		if len(parts) != 2 {
			return page, ErrInvalid
		}
		if parts[1] != identity {
			return page, ErrHistoryChanged
		}
		offset, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || offset < 0 || offset > info.Size() {
			return page, ErrInvalid
		}
	}
	if offset > 0 {
		var prior [1]byte
		if _, err = f.ReadAt(prior[:], offset-1); err != nil {
			return page, err
		}
		if prior[0] != '\n' && offset != info.Size() {
			return page, ErrInvalid
		}
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return page, err
	}
	r := bufio.NewReaderSize(&contextReader{ctx: ctx, r: f}, 64<<10)
	limit = pageLimit(limit)
	size := 0
	for len(page.Items) < limit && size < 1<<20 && offset < info.Size() {
		line, e := r.ReadSlice('\n')
		if e == bufio.ErrBufferFull {
			return page, fmt.Errorf("legacy economics record exceeds 64 KiB display limit; use the preserved raw download")
		}
		if e != nil && e != io.EOF {
			return page, e
		}
		var object map[string]json.RawMessage
		valid := utf8.Valid(line) && json.Unmarshal(line, &object) == nil && object != nil
		page.Items = append(page.Items, LegacyEconomicsEntry{Offset: offset, Length: len(line), Complete: e != io.EOF, ValidObject: valid, Raw: string(line)})
		offset += int64(len(line))
		size += len(line)
		if e == io.EOF {
			break
		}
	}
	afterInfo, err := f.Stat()
	if err != nil {
		return page, err
	}
	if afterInfo.Size() != info.Size() || !afterInfo.ModTime().Equal(info.ModTime()) {
		return page, ErrHistoryChanged
	}
	if offset < info.Size() {
		page.NextCursor = encodeKey(strconv.FormatInt(offset, 10)+"|"+identity, "legacy-economics")
	}
	return page, nil
}
