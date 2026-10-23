// Package gce is the small part of the Compute Engine API lockstep
// needs: list a job's instances with their upcoming maintenance, and ask
// for maintenance to start now.
package gce

import (
	"fmt"
	"strings"
	"time"
)

// Instance is a VM as lockstep sees it.
type Instance struct {
	Name   string
	Zone   string
	Status string // RUNNING, TERMINATED, STOPPING, REPAIRING, ...
	Labels map[string]string
	// Maintenance is nil when none is scheduled.
	Maintenance *Maintenance
}

// Running reports whether the instance is up.
func (i Instance) Running() bool { return i.Status == "RUNNING" }

// Maintenance is resourceStatus.upcomingMaintenance.
type Maintenance struct {
	Type          string // SCHEDULED, UNSCHEDULED, MULTIPLE
	Status        string // PENDING, ONGOING
	CanReschedule bool
	// WindowStart is when the maintenance is scheduled to begin, and
	// LatestStart the latest Compute Engine will start it. Either may be
	// zero when the API leaves it out.
	WindowStart time.Time
	LatestStart time.Time
	WindowEnd   time.Time
	Reasons     []string
}

// Pending reports whether the maintenance hasn't started.
func (m *Maintenance) Pending() bool { return m != nil && m.Status == "PENDING" }

// Emergency reports whether Compute Engine scheduled it because hardware
// is failing, rather than as routine work.
func (m *Maintenance) Emergency() bool {
	if m.Type == "UNSCHEDULED" {
		return true
	}
	for _, r := range m.Reasons {
		if strings.HasPrefix(r, "FAILURE_") {
			return true
		}
	}
	return false
}

// Deadline is the latest the instance can count on staying up: the
// latest start if there is one, the window start otherwise. Zero if the
// API gave neither.
func (m *Maintenance) Deadline() time.Time {
	if !m.LatestStart.IsZero() {
		return m.LatestStart
	}
	return m.WindowStart
}

// The JSON shapes, as the API sends them.

type instanceJSON struct {
	Name           string            `json:"name"`
	Zone           string            `json:"zone"`
	Status         string            `json:"status"`
	Labels         map[string]string `json:"labels"`
	ResourceStatus *struct {
		UpcomingMaintenance *maintenanceJSON `json:"upcomingMaintenance"`
	} `json:"resourceStatus"`
	// Older responses and the metadata server put it at the top level.
	UpcomingMaintenance *maintenanceJSON `json:"upcomingMaintenance"`
}

type maintenanceJSON struct {
	Type                  string   `json:"type"`
	MaintenanceStatus     string   `json:"maintenanceStatus"`
	CanReschedule         bool     `json:"canReschedule"`
	WindowStartTime       string   `json:"windowStartTime"`
	LatestWindowStartTime string   `json:"latestWindowStartTime"`
	WindowEndTime         string   `json:"windowEndTime"`
	MaintenanceReasons    []string `json:"maintenanceReasons"`
}

func (j instanceJSON) instance() (Instance, error) {
	in := Instance{Name: j.Name, Zone: lastPart(j.Zone), Status: j.Status, Labels: j.Labels}
	mj := j.UpcomingMaintenance
	if j.ResourceStatus != nil && j.ResourceStatus.UpcomingMaintenance != nil {
		mj = j.ResourceStatus.UpcomingMaintenance
	}
	if mj != nil {
		m, err := mj.maintenance()
		if err != nil {
			return in, fmt.Errorf("instance %s: %w", j.Name, err)
		}
		in.Maintenance = m
	}
	return in, nil
}

func (j maintenanceJSON) maintenance() (*Maintenance, error) {
	m := &Maintenance{Type: j.Type, Status: j.MaintenanceStatus, CanReschedule: j.CanReschedule, Reasons: j.MaintenanceReasons}
	for _, f := range []struct {
		in  string
		out *time.Time
	}{{j.WindowStartTime, &m.WindowStart}, {j.LatestWindowStartTime, &m.LatestStart}, {j.WindowEndTime, &m.WindowEnd}} {
		if f.in == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, f.in)
		if err != nil {
			return nil, fmt.Errorf("maintenance time %q: %w", f.in, err)
		}
		*f.out = t
	}
	return m, nil
}

// lastPart turns a zone URL into its name.
func lastPart(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}
