package desktop

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type DirectiveTarget struct {
	Path        string `json:"path"`
	Label       string `json:"label"`
	Name        string `json:"name"`
	PlantedAt   int64  `json:"plantedAt"`
	State       string `json:"state,omitempty"`
	AppliedHash string `json:"appliedHash,omitempty"`
}
type Directive struct {
	expectedBlockHash  string
	PendingMeasurement *Measurement      `json:"pendingMeasurement,omitempty"`
	ID                 string            `json:"id"`
	Title              string            `json:"title"`
	Body               string            `json:"body"`
	InsightKey         string            `json:"insightKey,omitempty"`
	Topic              string            `json:"topic"`
	CreatedAt          int64             `json:"createdAt"`
	ReviewEveryDays    int               `json:"reviewEveryDays"`
	LastReviewedAt     int64             `json:"lastReviewedAt"`
	Targets            []DirectiveTarget `json:"targets"`
}

func markers(id string) (string, string) {
	return "<!-- mission-control:directive:" + id + " -->", "<!-- /mission-control:directive:" + id + " -->"
}
func directiveBlock(d Directive) string {
	start, end := markers(d.ID)
	return fmt.Sprintf("\n\n%s\n## 🛰 Standing order: %s\n_Planted by Agent Mission Control on %s. Retire it from the dashboard rather than hand-editing this block._\n\n%s\n%s\n", start, d.Title, time.UnixMilli(d.CreatedAt).UTC().Format("2006-01-02"), strings.TrimSpace(d.Body), end)
}

func directiveBounds(content string, d Directive) (int, int, string) {
	start, end := markers(d.ID)
	a, b := strings.Count(content, start), strings.Count(content, end)
	if a == 0 && b == 0 {
		return 0, 0, "absent"
	}
	if a != 1 || b != 1 {
		return 0, 0, "malformed"
	}
	s, e := strings.Index(content, start), strings.Index(content, end)
	if e < s {
		return 0, 0, "malformed"
	}
	e += len(end)
	expected := d.expectedBlockHash
	if expected == "" {
		expected = directiveHash(directiveBlock(d))
	}
	if directiveHash(content[s:e]) != expected {
		return s, e, "modified"
	}
	return s, e, "intact"
}

func targetDirective(d Directive, target DirectiveTarget) Directive {
	d.expectedBlockHash = target.AppliedHash
	return d
}

func directiveHash(block string) string {
	return hash([]byte(strings.TrimSpace(strings.ReplaceAll(block, "\r\n", "\n"))))
}
func (m *Manager) directiveItems() ([]Directive, error) {
	items := []Directive{}
	err := m.load("directives", &items)
	return items, err
}
func (m *Manager) directives(r *http.Request, b input) (any, error) {
	var planting *plantOperation
	if r.Method == http.MethodPost && b.Op == "plant" && b.OperationID != "" {
		var prior map[string]any
		var err error
		planting, prior, err = m.loadPlantOperation(b)
		if err != nil {
			return nil, err
		}
		if prior != nil {
			return prior, nil
		}
		if planting.id != "" {
			b.Op, b.ID = "plant-existing", planting.id
		}
	}
	if r.Method == http.MethodPost && b.Op == "reviewed" && (b.OperationID != "" || b.ExpectedHash != "") {
		return m.reviewDirective(b)
	}
	if r.Method == http.MethodGet && r.URL.Query().Get("registry") == "1" {
		targets, err := m.directiveTargets()
		if err != nil {
			return nil, err
		}
		roots, err := m.rootList()
		return map[string]any{"targets": targets, "roots": roots}, err
	}
	if r.Method == http.MethodGet && (r.URL.Query().Has("limit") || r.URL.Query().Has("id")) {
		return m.directivePage(r)
	}
	items, err := m.directiveItems()
	if err != nil {
		return nil, err
	}
	if r.Method == http.MethodGet {
		targets, err := m.directiveTargets()
		if err != nil {
			return nil, err
		}
		roots, err := m.rootList()
		return map[string]any{"items": items, "targets": targets, "roots": roots}, err
	}
	if b.Op == "add-root" || b.Op == "remove-root" {
		return m.changeRoot(b, items)
	}
	index := -1
	for i := range items {
		if items[i].ID == b.ID {
			index = i
			break
		}
	}
	if b.Op == "plant" {
		if strings.TrimSpace(b.Body) == "" || len(b.Body) > 20000 {
			return nil, fail(400, "directive body is required and must be under 20,000 bytes")
		}
		if len(items) >= 200 {
			return nil, fail(400, "directive registry is full")
		}
		title := clean(b.Title, 100)
		if title == "" {
			title = "Untitled directive"
		}
		days := b.ReviewEveryDays
		if days < 1 {
			days = 30
		}
		if days > 3650 {
			days = 3650
		}
		topic := clean(b.Topic, 40)
		if topic == "" {
			topic = "general"
		}
		items = append(items, Directive{ID: randomID("dir_"), Title: title, Body: b.Body, InsightKey: clean(b.InsightKey, 200), Topic: topic, CreatedAt: now(), ReviewEveryDays: days, LastReviewedAt: now(), Targets: []DirectiveTarget{}})
		index = len(items) - 1
	} else if index < 0 {
		return nil, fail(404, "directive was not found")
	}
	d := &items[index]
	if planting != nil && planting.id != "" && directiveHash(directiveBlock(*d)) != planting.blockHash {
		return nil, fail(409, "pending planting rule changed; inspect before recovering this operation")
	}
	if b.ExpectedHash != "" && viewDirective(*d).StateHash != b.ExpectedHash {
		return nil, fail(409, "standing order changed; reload and inspect it before changing its files")
	}
	switch b.Op {
	case "plant", "plant-existing":
		if len(b.Targets) == 0 || len(b.Targets) > 40 {
			return nil, fail(400, "pick between 1 and 40 targets")
		}
		allowed, err := m.directiveTargets()
		if err != nil {
			return nil, err
		}
		byID := map[string]Item{}
		for _, item := range allowed {
			byID[item.ID] = item
		}
		results := []map[string]any{}
		// Save the owner-created directive before changing targets so a crash can
		// never leave a planted marker whose directive body has been forgotten.
		if planting != nil && planting.id == "" {
			intent := map[string]any{"id": d.ID, "operationId": b.OperationID, "blockHash": directiveHash(directiveBlock(*d))}
			err = m.saveOperation("directives", items, "directive-intent", planting.key+":intent", &localOperationReceipt{RequestHash: planting.requestHash, Result: intent})
		} else if planting == nil {
			err = m.save("directives", items, "directive-intent")
		}
		if err != nil {
			return nil, err
		}
		for _, id := range b.Targets {
			item, ok := byID[id]
			if !ok {
				results = append(results, map[string]any{"id": clean(id, 60), "status": "unknown-target"})
				continue
			}
			targetIndex := -1
			for i, t := range d.Targets {
				if norm(t.Path) == norm(item.Path) {
					targetIndex = i
					break
				}
			}
			if targetIndex < 0 && len(d.Targets) >= 40 {
				results = append(results, map[string]any{"label": item.Label, "status": "full"})
				continue
			}
			cur, e := readBounded(item.Path)
			if e != nil && !os.IsNotExist(e) {
				results = append(results, map[string]any{"label": item.Label, "status": "error", "error": e.Error()})
				continue
			}
			_, _, blockState := directiveBounds(string(cur), *d)
			status := "already"
			// Persist the selected target before a filesystem mutation. A crash or
			// failed completion transaction must not orphan a planted block.
			if targetIndex < 0 {
				d.Targets = append(d.Targets, DirectiveTarget{Path: item.Path, Label: item.Label, Name: item.Name, State: "pending"})
				targetIndex = len(d.Targets) - 1
			} else {
				d.Targets[targetIndex].State = "pending"
			}
			if err = m.save("directives", items, "directive-target-intent"); err != nil {
				return nil, err
			}
			if blockState == "malformed" || blockState == "modified" {
				e = fail(409, "standing-order block changed or is incomplete; inspect it before planting again")
			} else if blockState == "absent" {
				e = m.writeFile(item, hash(cur), append(cur, []byte(directiveBlock(*d))...), "directive-plant")
				status = "planted"
			}
			result := map[string]any{"label": item.Label, "status": status}
			if e != nil {
				result["status"] = "error"
				result["error"] = e.Error()
			} else {
				d.Targets[targetIndex].State = "planted"
				d.Targets[targetIndex].AppliedHash = directiveHash(directiveBlock(*d))
				if d.Targets[targetIndex].PlantedAt == 0 {
					d.Targets[targetIndex].PlantedAt = now()
				}
				if err = m.save("directives", items, "directive-target"); err != nil {
					return nil, err
				}
			}
			results = append(results, result)
		}
		if planting != nil {
			result := map[string]any{"ok": true, "id": d.ID, "operationId": b.OperationID, "item": viewDirective(*d), "results": results}
			err = m.saveOperation("directives", items, "directive-plant-complete", planting.key, &localOperationReceipt{RequestHash: planting.requestHash, Result: result})
			return result, err
		}
		if b.ExpectedHash != "" {
			return map[string]any{"ok": true, "id": d.ID, "item": viewDirective(*d), "results": results}, nil
		}
		return map[string]any{"ok": true, "items": items, "results": results}, nil
	case "check":
		statuses := []map[string]any{}
		for _, target := range d.Targets {
			status := "missing-file"
			if m.validateFile(target.Path) == nil {
				if content, e := readBounded(target.Path); e == nil {
					status = "drifted"
					_, _, blockState := directiveBounds(string(content), targetDirective(*d, target))
					if blockState == "intact" {
						status = "ok"
					} else if blockState != "absent" {
						status = blockState
					}
				}
			}
			statuses = append(statuses, map[string]any{"path": target.Path, "label": target.Label, "status": status, "registryState": target.State})
		}
		return map[string]any{"ok": true, "statuses": statuses}, nil
	case "reviewed":
		d.LastReviewedAt = now()
		err = m.save("directives", items, "directive-reviewed")
		return map[string]any{"ok": true, "items": items}, err
	case "delete":
		items = append(items[:index], items[index+1:]...)
		err = m.save("directives", items, "directive-forget")
		return map[string]any{"ok": true, "items": items}, err
	case "retire":
		results := []map[string]any{}
		needsCommit := []string{}
		retained := []DirectiveTarget{}
		for _, target := range d.Targets {
			status, e := m.replaceDirective(target, targetDirective(*d, target), "")
			result := map[string]any{"label": target.Label, "status": status}
			if e != nil {
				result["status"] = "error"
				result["error"] = e.Error()
				retained = append(retained, target)
			} else if root, _ := gitRoot(r.Context(), target.Path); root != "" {
				needsCommit = append(needsCommit, target.Label)
			}
			results = append(results, result)
		}
		if len(retained) == 0 {
			items = append(items[:index], items[index+1:]...)
		} else {
			d.Targets = retained
		}
		err = m.save("directives", items, "directive-retire")
		if b.ExpectedHash != "" {
			return map[string]any{"ok": true, "id": b.ID, "removed": len(retained) == 0, "results": results, "needsCommit": needsCommit}, err
		}
		return map[string]any{"ok": true, "items": items, "results": results, "needsCommit": needsCommit}, err
	case "remeasure", "remeasure-preview":
		if d.Topic != "model-tiering" {
			return nil, fail(400, "only model-tiering directives can be remeasured")
		}
		var measurement Measurement
		if b.Op == "remeasure" && b.Key != "" {
			if len(b.Key) > 128 {
				return nil, fail(400, "invalid measurement preview identity")
			}
			var preview measurementPreview
			if err = m.load("measurement-preview:"+b.Key, &preview); err != nil {
				return nil, err
			}
			if preview.DirectiveID != d.ID || preview.StateHash == "" || preview.StateHash != b.ExpectedHash {
				return nil, fail(409, "measurement preview is missing or does not match the reviewed rule state")
			}
			measurement = preview.Measurement
		} else if d.PendingMeasurement != nil {
			measurement = *d.PendingMeasurement
		} else {
			if m.opts.Remeasure == nil && m.opts.RemeasureDirective == nil {
				return nil, fail(503, "fleet measurements are not available in this desktop process")
			}
			var e error
			if m.opts.RemeasureDirective != nil {
				measurement, e = m.opts.RemeasureDirective(r.Context(), d.Body)
			} else {
				measurement, e = m.opts.Remeasure(r.Context())
			}
			if e != nil {
				return nil, e
			}
		}
		if measurement.Subs < 200 {
			return nil, fail(400, "at least 200 measured subagents are required")
		}
		if len(measurement.Body) > 20000 {
			return nil, fail(400, "measured directive is too long")
		}
		if strings.TrimSpace(measurement.Body) == "" || measurement.Sessions < 0 {
			return nil, fail(400, "measurement body or session count is invalid")
		}
		if b.Op == "remeasure-preview" {
			id := randomID("measure_")
			preview := measurementPreview{DirectiveID: d.ID, StateHash: viewDirective(*d).StateHash, Measurement: measurement}
			if err = m.save("measurement-preview:"+id, preview, "directive-remeasure-preview"); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "id": d.ID, "previewId": id, "stateHash": preview.StateHash, "body": measurement.Body, "measuredFrom": map[string]int{"sessions": measurement.Sessions, "subs": measurement.Subs}}, nil
		}
		previous := *d
		for i := range d.Targets {
			if d.Targets[i].AppliedHash == "" {
				d.Targets[i].AppliedHash = directiveHash(directiveBlock(previous))
			}
			d.Targets[i].State = "pending"
		}
		d.Body = measurement.Body
		d.PendingMeasurement = &measurement
		d.LastReviewedAt = now()
		if err = m.save("directives", items, "directive-remeasure-intent"); err != nil {
			return nil, err
		}
		results := []map[string]any{}
		for i, target := range d.Targets {
			status, e := m.replaceDirective(target, targetDirective(previous, target), directiveBlock(*d))
			result := map[string]any{"label": target.Label, "path": target.Path, "status": status}
			if e != nil {
				result["status"] = "error"
				result["error"] = e.Error()
			} else if status != "missing-file" && status != "not-present" {
				d.Targets[i].AppliedHash = directiveHash(directiveBlock(*d))
				d.Targets[i].State = "planted"
				if err = m.save("directives", items, "directive-remeasure-target"); err != nil {
					return nil, err
				}
			}
			results = append(results, result)
		}
		complete := true
		for _, target := range d.Targets {
			if target.State != "planted" {
				complete = false
				break
			}
		}
		if complete {
			d.PendingMeasurement = nil
			if err = m.save("directives", items, "directive-remeasure-complete"); err != nil {
				return nil, err
			}
		}
		return map[string]any{"ok": true, "id": d.ID, "complete": complete, "results": results, "measuredFrom": map[string]int{"sessions": measurement.Sessions, "subs": measurement.Subs}, "body": d.Body}, nil
	case "git-status":
		states := []map[string]any{}
		for _, target := range d.Targets {
			if b.Path != "" && norm(target.Path) != norm(b.Path) {
				continue
			}
			states = append(states, m.gitStatus(r.Context(), *d, target))
		}
		if b.Path != "" && len(states) == 0 {
			return nil, fail(400, "that file is not one of this directive's targets")
		}
		return map[string]any{"ok": true, "states": states}, m.audit("directive-git-status", "", "read", map[string]string{"directiveId": d.ID})
	case "git-commit", "git-push":
		var target *DirectiveTarget
		for i := range d.Targets {
			if norm(d.Targets[i].Path) == norm(b.Path) {
				target = &d.Targets[i]
				break
			}
		}
		if target == nil {
			return nil, fail(400, "that file is not one of this directive's targets")
		}
		return m.gitMutation(r.Context(), b.Op, *d, *target)
	default:
		return nil, fail(400, "unknown directive action")
	}
}
func (m *Manager) replaceDirective(target DirectiveTarget, d Directive, replacement string) (string, error) {
	if err := m.validateFile(target.Path); err != nil {
		return "error", err
	}
	content, err := readBounded(target.Path)
	if os.IsNotExist(err) {
		return "missing-file", nil
	}
	if err != nil {
		return "error", err
	}
	s, e, blockState := directiveBounds(string(content), d)
	if blockState == "absent" {
		return "not-present", nil
	}
	if blockState == "malformed" {
		return "error", fail(409, "standing-order markers are incomplete or duplicated; inspect before changing this file")
	}
	// Both completed targets and file-before-commit targets may already contain
	// this exact desired block. Do not rewrite them or add whitespace on replay.
	if replacement != "" && directiveHash(string(content[s:e])) == directiveHash(replacement) {
		return "already", nil
	}
	if blockState == "modified" {
		return "error", fail(409, "standing-order content changed; compare the file before replacing or retiring it")
	}
	next := string(content[:s]) + replacement + string(content[e:])
	status := "updated"
	if replacement == "" {
		status = "retired"
	}
	err = m.writeFile(Item{Path: target.Path, Name: target.Name}, hash(content), []byte(next), "directive-"+status)
	return status, err
}
func (m *Manager) changeRoot(b input, items []Directive) (any, error) {
	roots, err := m.rootList()
	if err != nil {
		return nil, err
	}
	stranded := 0
	if b.Op == "add-root" {
		p, err := m.validateRoot(b.Path)
		if err != nil {
			return nil, fail(400, err.Error())
		}
		found := false
		for _, root := range roots {
			if norm(root.Path) == norm(p) {
				found = true
			}
		}
		if !found {
			if len(roots) >= 50 {
				return nil, fail(400, "at most 50 added roots are supported")
			}
			roots = append(roots, Root{p, filepath.Base(p), now(), true})
		}
	} else {
		if !filepath.IsAbs(b.Path) || len(b.Path) > 400 {
			return nil, fail(400, "a full root path is required")
		}
		kept := []Root{}
		for _, root := range roots {
			if norm(root.Path) != norm(b.Path) {
				kept = append(kept, root)
			}
		}
		roots = kept
		for _, d := range items {
			for _, t := range d.Targets {
				if under(b.Path, t.Path) {
					stranded++
					break
				}
			}
		}
	}
	if err = m.save("roots", roots, "directive-"+b.Op); err != nil {
		return nil, err
	}
	targets, err := m.directiveTargets()
	return map[string]any{"ok": true, "roots": roots, "targets": targets, "stranded": stranded}, err
}
