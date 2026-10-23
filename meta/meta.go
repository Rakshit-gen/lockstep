// Package meta reads a VM's own maintenance from the Compute Engine
// metadata server, and runs a hook when its host is about to take it.
package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Rakshit-gen/lockstep/gce"
)

// Client talks to the metadata server.
type Client struct {
	// Host is host:port, metadata.google.internal unless
	// GCE_METADATA_HOST says otherwise.
	Host string
	HTTP *http.Client
}

// New makes a client for this VM.
func New() *Client {
	h := os.Getenv("GCE_METADATA_HOST")
	if h == "" {
		h = "metadata.google.internal"
	}
	// No timeout: wait_for_change holds the request open until the value
	// changes.
	return &Client{Host: h, HTTP: &http.Client{}}
}

// ErrNotFound is a missing metadata key.
var ErrNotFound = fmt.Errorf("meta: not found")

func (c *Client) get(ctx context.Context, path string, q url.Values) (string, string, error) {
	u := "http://" + c.Host + "/computeMetadata/v1/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", "", ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return "", "", fmt.Errorf("meta: %s: %s", path, resp.Status)
	}
	return strings.TrimSpace(string(b)), resp.Header.Get("ETag"), nil
}

// MaintenanceEvent returns instance/maintenance-event: NONE, or
// TERMINATE_ON_HOST_MAINTENANCE when the host is about to stop this VM.
// With a last ETag it waits for the value to change first.
func (c *Client) MaintenanceEvent(ctx context.Context, etag string) (string, string, error) {
	q := url.Values{}
	if etag != "" {
		q.Set("wait_for_change", "true")
		q.Set("last_etag", etag)
	}
	return c.get(ctx, "instance/maintenance-event", q)
}

// Upcoming returns instance/upcoming-maintenance, or nil when nothing is
// scheduled.
func (c *Client) Upcoming(ctx context.Context) (*gce.Maintenance, error) {
	v, _, err := c.get(ctx, "instance/upcoming-maintenance", nil)
	if err == ErrNotFound || (err == nil && v == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseUpcoming([]byte(v))
}

// ParseUpcoming reads the metadata server's form of upcoming maintenance,
// which is snake_case with can_reschedule as a string.
func ParseUpcoming(b []byte) (*gce.Maintenance, error) {
	var j struct {
		Type          string   `json:"type"`
		Status        string   `json:"maintenance_status"`
		CanReschedule any      `json:"can_reschedule"`
		Start         string   `json:"window_start_time"`
		Latest        string   `json:"latest_window_start_time"`
		End           string   `json:"window_end_time"`
		Reasons       []string `json:"maintenance_reasons"`
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("meta: upcoming-maintenance: %w", err)
	}
	m := &gce.Maintenance{Type: j.Type, Status: j.Status, Reasons: j.Reasons}
	switch v := j.CanReschedule.(type) {
	case bool:
		m.CanReschedule = v
	case string:
		m.CanReschedule = strings.EqualFold(v, "true")
	}
	for _, f := range []struct {
		in  string
		out *time.Time
	}{{j.Start, &m.WindowStart}, {j.Latest, &m.LatestStart}, {j.End, &m.WindowEnd}} {
		if f.in == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, f.in)
		if err != nil {
			return nil, fmt.Errorf("meta: upcoming-maintenance time %q: %w", f.in, err)
		}
		*f.out = t
	}
	return m, nil
}

// Watch calls fn each time instance/maintenance-event changes, starting
// with its current value, until ctx ends. Errors are retried with backoff
// and passed to logf.
func (c *Client) Watch(ctx context.Context, fn func(event string), logf func(string, ...any)) error {
	etag, last := "", ""
	backoff := time.Second
	for {
		v, tag, err := c.MaintenanceEvent(ctx, etag)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			logf("maintenance-event: %v; retrying in %s", err, backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, time.Minute)
			continue
		}
		backoff = time.Second
		etag = tag
		if v != last {
			last = v
			fn(v)
		}
	}
}
