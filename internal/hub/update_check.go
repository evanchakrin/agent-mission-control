package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const versionSource = "https://raw.githubusercontent.com/evanchakrin/agent-mission-control/main/package.json"

type updateStatus struct {
	Current         string    `json:"current"`
	Latest          string    `json:"latest,omitempty"`
	UpdateAvailable bool      `json:"updateAvailable"`
	State           string    `json:"state"`
	CheckedAt       time.Time `json:"checkedAt,omitempty"`
	Source          string    `json:"source"`
}

type updateChecker struct {
	mu     sync.Mutex
	value  updateStatus
	next   time.Time
	client *http.Client
}

var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// The upstream package advertises stable releases. Candidate builds are not
// mistaken for newer releases just because they use a different version string.
func newerStable(latest, current string) (bool, bool) {
	base, suffix, _ := strings.Cut(current, "-")
	base, _, _ = strings.Cut(base, "+")
	if len(latest) > 64 || len(current) > 128 || !releaseVersion.MatchString(latest) || !releaseVersion.MatchString(base) {
		return false, false
	}
	a, b := strings.Split(latest, "."), strings.Split(base, ".")
	for i := 0; i < 3; i++ {
		x, e1 := strconv.ParseUint(a[i], 10, 64)
		y, e2 := strconv.ParseUint(b[i], 10, 64)
		if e1 != nil || e2 != nil {
			return false, false
		}
		if x != y {
			return x > y, true
		}
	}
	return suffix != "", true
}

func (c *updateChecker) check(ctx context.Context, current string) updateStatus {
	if !c.mu.TryLock() {
		return updateStatus{Current: current, State: "checking", Source: versionSource}
	}
	defer c.mu.Unlock()
	now := time.Now().UTC()
	if now.Before(c.next) {
		return c.value
	}
	c.value = updateStatus{Current: current, State: "unavailable", CheckedAt: now, Source: versionSource}
	c.next = now.Add(15 * time.Minute) // Outages must not turn UI refreshes into retries.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, versionSource, nil)
	if err != nil {
		return c.value
	}
	client := c.client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := client.Do(req)
	if err != nil {
		return c.value
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return c.value
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return c.value
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return c.value
	}
	newer, valid := newerStable(pkg.Version, current)
	if !valid {
		return c.value
	}
	c.value.Latest = pkg.Version
	c.value.UpdateAvailable = newer
	c.value.State = "ready"
	c.next = now.Add(6 * time.Hour)
	return c.value
}
