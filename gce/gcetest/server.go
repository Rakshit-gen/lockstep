// Package gcetest is a fake Compute Engine that answers the calls
// lockstep makes, for tests and for trying the controller without a
// project.
package gcetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Instance is a fake VM.
type Instance struct {
	Name, Zone, Status string
	Labels             map[string]string
	// Maintenance, when set, is served as upcomingMaintenance.
	Maintenance map[string]any
}

// Server holds fake instances and records performMaintenance calls.
type Server struct {
	*httptest.Server

	mu        sync.Mutex
	instances map[string]*Instance
	calls     []string
	// PageSize splits listings into pages, to exercise paging.
	PageSize int
	// Fail makes performMaintenance return this HTTP status once per
	// instance name, then succeed.
	Fail map[string]int
}

// New starts a server. Its URL plus /compute/v1 is the client's base.
func New() *Server {
	s := &Server{instances: map[string]*Instance{}, Fail: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Base is the URL to give a gce.Client.
func (s *Server) Base() string { return s.URL + "/compute/v1" }

// Put adds or replaces an instance.
func (s *Server) Put(in Instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.Status == "" {
		in.Status = "RUNNING"
	}
	s.instances[in.Name] = &in
}

// Get returns a copy of an instance.
func (s *Server) Get(name string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[name]
	if !ok {
		return Instance{}, false
	}
	return *in, true
}

// Finish ends an instance's maintenance: it is running again with
// nothing scheduled.
func (s *Server) Finish(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in, ok := s.instances[name]; ok {
		in.Status, in.Maintenance = "RUNNING", nil
	}
}

// Calls lists the instances performMaintenance was called for, in order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// Pending gives a maintenance block for an instance.
func Pending(start time.Time, reschedule bool, reasons ...string) map[string]any {
	m := map[string]any{
		"type":              "SCHEDULED",
		"maintenanceStatus": "PENDING",
		"canReschedule":     reschedule,
		"windowStartTime":   start.UTC().Format(time.RFC3339),
		"windowEndTime":     start.Add(4 * time.Hour).UTC().Format(time.RFC3339),
	}
	if len(reasons) > 0 {
		m["maintenanceReasons"] = reasons
		if strings.HasPrefix(reasons[0], "FAILURE_") {
			m["type"] = "UNSCHEDULED"
		}
	}
	return m
}

var (
	listPath    = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/aggregated/instances$`)
	performPath = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/zones/([^/]+)/instances/([^/]+)/performMaintenance$`)
	waitPath    = regexp.MustCompile(`^/compute/v1/projects/([^/]+)/zones/([^/]+)/operations/([^/]+)/wait$`)
	labelFilter = regexp.MustCompile(`^labels\.([a-z0-9_-]+)\s*=\s*"?([^"]*)"?$`)
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		apiError(w, 401, "no token")
		return
	}
	switch {
	case r.Method == http.MethodGet && listPath.MatchString(r.URL.Path):
		s.list(w, r)
	case r.Method == http.MethodPost && performPath.MatchString(r.URL.Path):
		m := performPath.FindStringSubmatch(r.URL.Path)
		s.perform(w, m[2], m[3])
	case r.Method == http.MethodPost && waitPath.MatchString(r.URL.Path):
		m := waitPath.FindStringSubmatch(r.URL.Path)
		json.NewEncoder(w).Encode(map[string]any{"name": m[3], "zone": "zones/" + m[2], "status": "DONE"})
	default:
		apiError(w, 404, "no route for "+r.Method+" "+r.URL.Path)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var key, val string
	if f := r.URL.Query().Get("filter"); f != "" {
		m := labelFilter.FindStringSubmatch(strings.TrimSpace(f))
		if m == nil {
			apiError(w, 400, "the fake only understands labels.KEY = \"VALUE\" filters")
			return
		}
		key, val = m[1], m[2]
	}
	var names []string
	for n, in := range s.instances {
		if key == "" || in.Labels[key] == val {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	start := 0
	if t := r.URL.Query().Get("pageToken"); t != "" {
		start, _ = strconv.Atoi(t)
	}
	end := len(names)
	if s.PageSize > 0 && start+s.PageSize < end {
		end = start + s.PageSize
	}
	items := map[string]map[string][]map[string]any{}
	for _, n := range names[start:end] {
		in := s.instances[n]
		j := map[string]any{"name": in.Name, "zone": "https://www.googleapis.com/compute/v1/projects/p/zones/" + in.Zone, "status": in.Status, "labels": in.Labels}
		if in.Maintenance != nil {
			j["resourceStatus"] = map[string]any{"upcomingMaintenance": in.Maintenance}
		}
		scope := "zones/" + in.Zone
		if items[scope] == nil {
			items[scope] = map[string][]map[string]any{}
		}
		items[scope]["instances"] = append(items[scope]["instances"], j)
	}
	out := map[string]any{"items": items}
	if end < len(names) {
		out["nextPageToken"] = strconv.Itoa(end)
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) perform(w http.ResponseWriter, zone, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[name]
	if !ok || in.Zone != zone {
		apiError(w, 404, fmt.Sprintf("instance %s not found in %s", name, zone))
		return
	}
	if code := s.Fail[name]; code != 0 {
		delete(s.Fail, name)
		apiError(w, code, "injected failure")
		return
	}
	if in.Maintenance == nil || in.Maintenance["maintenanceStatus"] != "PENDING" {
		apiError(w, 400, "no pending maintenance on "+name)
		return
	}
	if in.Maintenance["canReschedule"] != true {
		apiError(w, 400, "maintenance on "+name+" can't be rescheduled")
		return
	}
	in.Maintenance["maintenanceStatus"] = "ONGOING"
	in.Status = "TERMINATED"
	s.calls = append(s.calls, name)
	json.NewEncoder(w).Encode(map[string]any{"name": "op-" + name, "zone": "zones/" + zone, "status": "RUNNING"})
}

func apiError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}
