package plan

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func ev(in time.Duration, resched bool) *Event {
	return &Event{Deadline: t0.Add(in), CanReschedule: resched}
}

func names(ns []Node) string {
	var s []string
	for _, n := range ns {
		s = append(s, n.Name)
	}
	return strings.Join(s, ",")
}

func TestRunningWaitsThenBatches(t *testing.T) {
	c := Config{Size: 3, Margin: 30 * time.Minute}
	nodes := []Node{
		{Name: "a", Up: true, InJob: true, Event: ev(48*time.Hour, true)},
		{Name: "b", Up: true, InJob: true, Event: ev(24*time.Hour, false)},
		{Name: "c", Up: true, InJob: true},
		{Name: "s", Up: true, Event: ev(72*time.Hour, true)},
	}
	a := Decide(State{Now: t0, Running: true, Nodes: nodes}, c)
	if a.Stop || names(a.Trigger) != "s" || !a.Wake.Equal(t0.Add(24*time.Hour-30*time.Minute)) {
		t.Fatalf("early: %+v", a)
	}
	a = Decide(State{Now: a.Wake, Running: true, Nodes: nodes[:3]}, c)
	// b can't be moved but forces the stop; a rides along.
	if !a.Stop || names(a.Trigger) != "a" || !strings.Contains(a.Why, "b") {
		t.Fatalf("at deadline: %+v", a)
	}
}

func TestRunningNothingPending(t *testing.T) {
	a := Decide(State{Now: t0, Running: true, Nodes: []Node{{Name: "a", Up: true, InJob: true}}}, Config{Size: 1})
	if a.Stop || len(a.Trigger) > 0 || !a.Wake.IsZero() {
		t.Fatalf("%+v", a)
	}
}

func TestDownPiggybacks(t *testing.T) {
	nodes := []Node{
		{Name: "a", Up: true, Event: ev(80*24*time.Hour, true)},
		{Name: "b", Up: true, Event: ev(2*24*time.Hour, true)},
		{Name: "c", Up: true},
		{Name: "d", Up: false},
	}
	// No spares: only what is due within the horizon goes.
	a := Decide(State{Now: t0, Nodes: nodes}, Config{Size: 3, Horizon: 7 * 24 * time.Hour})
	if a.Stop || names(a.Trigger) != "b" {
		t.Fatalf("horizon: %+v", a)
	}
	a = Decide(State{Now: t0, Nodes: nodes}, Config{Size: 3})
	if names(a.Trigger) != "a,b" {
		t.Fatalf("no horizon: %+v", a)
	}
	// One node is enough: the spares cover both, so both go whatever the
	// horizon.
	a = Decide(State{Now: t0, Nodes: nodes}, Config{Size: 1, Horizon: time.Hour})
	if names(a.Trigger) != "a,b" {
		t.Fatalf("spares: %+v", a)
	}
}
