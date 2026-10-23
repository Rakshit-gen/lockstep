package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake serves maintenance-event with ETags and wait_for_change.
type fake struct {
	mu       sync.Mutex
	val      string
	ver      int
	changed  chan struct{}
	upcoming string
}

func (f *fake) set(v string) {
	f.mu.Lock()
	f.val, f.ver = v, f.ver+1
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Metadata-Flavor") != "Google" {
		w.WriteHeader(403)
		return
	}
	switch r.URL.Path {
	case "/computeMetadata/v1/instance/upcoming-maintenance":
		if f.upcoming == "" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(f.upcoming))
	case "/computeMetadata/v1/instance/maintenance-event":
		f.mu.Lock()
		ch, ver := f.changed, f.ver
		f.mu.Unlock()
		if r.URL.Query().Get("wait_for_change") == "true" && r.URL.Query().Get("last_etag") == strconv.Itoa(ver) {
			select {
			case <-ch:
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		w.Header().Set("ETag", strconv.Itoa(f.ver))
		w.Write([]byte(f.val))
		f.mu.Unlock()
	default:
		w.WriteHeader(404)
	}
}

func TestWatch(t *testing.T) {
	f := &fake{val: "NONE", changed: make(chan struct{})}
	s := httptest.NewServer(f)
	defer s.Close()
	c := &Client{Host: strings.TrimPrefix(s.URL, "http://"), HTTP: http.DefaultClient}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan string, 4)
	go c.Watch(ctx, func(e string) { got <- e }, t.Logf)
	if e := <-got; e != "NONE" {
		t.Fatalf("first %q", e)
	}
	time.Sleep(50 * time.Millisecond)
	f.set("TERMINATE_ON_HOST_MAINTENANCE")
	select {
	case e := <-got:
		if e != "TERMINATE_ON_HOST_MAINTENANCE" {
			t.Fatalf("got %q", e)
		}
	case <-ctx.Done():
		t.Fatal("no change seen")
	}
}

func TestUpcoming(t *testing.T) {
	f := &fake{changed: make(chan struct{})}
	s := httptest.NewServer(f)
	defer s.Close()
	c := &Client{Host: strings.TrimPrefix(s.URL, "http://"), HTTP: http.DefaultClient}
	if m, err := c.Upcoming(context.Background()); m != nil || err != nil {
		t.Fatalf("none scheduled: %v %v", m, err)
	}
	// The example from the AI Hypercomputer host events page.
	f.upcoming = `{"can_reschedule": "true", "latest_window_start_time": "2024-12-01T19:00:01Z",
		"maintenance_reasons": ["PLANNED_UPDATE", "PLANNED_NETWORK_UPDATE"], "maintenance_status": "PENDING",
		"type": "SCHEDULED", "window_end_time": "2024-12-01T21:00:01Z", "window_start_time": "2024-12-01T19:00:01Z"}`
	m, err := c.Upcoming(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !m.Pending() || !m.CanReschedule || m.Emergency() || len(m.Reasons) != 2 || m.Deadline().Format(time.RFC3339) != "2024-12-01T19:00:01Z" {
		t.Fatalf("%+v", m)
	}
}
