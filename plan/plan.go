// Package plan decides when a gang job should stop for host maintenance
// and which nodes to send into it. A gang job restarts whenever any of its
// nodes goes away, so every maintenance event that hits it separately
// costs a full restart. The plan batches them: while the job runs it waits
// until the earliest event is about to hit, then stops the job once and
// starts every pending event it is allowed to start. When the job is
// already down, say after a failure, pending events ride along on the
// restart that is happening anyway.
package plan

import (
	"fmt"
	"sort"
	"time"
)

// Event is a node's pending maintenance.
type Event struct {
	// Deadline is the latest the node stays up.
	Deadline time.Time
	// CanReschedule is whether the event can be started early.
	CanReschedule bool
	Emergency     bool
}

// Node is a VM the job runs on or could run on.
type Node struct {
	Name, Zone string
	// Up is false while the node is in maintenance or otherwise gone.
	Up bool
	// InJob is whether the running job uses it. Ignored when the job is
	// down.
	InJob bool
	Event *Event
}

// State is what the controller sees.
type State struct {
	Now     time.Time
	Running bool
	Nodes   []Node
}

// Config holds the knobs.
type Config struct {
	// Size is how many nodes the job needs.
	Size int
	// Margin is how long before a deadline the job is stopped, enough to
	// write a checkpoint and stop cleanly.
	Margin time.Duration
	// Horizon limits which events are started early: only those due
	// within Horizon. Zero means all of them. With 90 days of notice on a
	// 90 day cadence a node shows its next event as soon as maintenance
	// ends, so without a horizon every stop would maintain every node.
	Horizon time.Duration
}

// Action is what to do now.
type Action struct {
	// Stop is set when the running job should checkpoint and stop.
	Stop bool
	// Trigger lists the nodes whose maintenance should start now.
	Trigger []Node
	// Wake is when to decide again if nothing changes first. Zero means
	// only on a change.
	Wake time.Time
	Why  string
}

// Decide says what to do in state s.
func Decide(s State, c Config) Action {
	if s.Running {
		return running(s, c)
	}
	return down(s, c)
}

func running(s State, c Config) Action {
	var a Action
	var first *Node
	up := 0
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if !n.Up {
			continue
		}
		up++
		if n.Event == nil {
			continue
		}
		if !n.InJob {
			// An idle node can go any time without touching the job.
			if c.early(s.Now, n) {
				a.Trigger = append(a.Trigger, *n)
			}
			continue
		}
		if first == nil || n.Event.Deadline.Before(first.Event.Deadline) {
			first = n
		}
	}
	if first == nil || s.Now.Before(first.Event.Deadline.Add(-c.Margin)) {
		if first != nil {
			a.Wake = first.Event.Deadline.Add(-c.Margin)
		}
		if len(a.Trigger) > 0 {
			a.Why = "idle nodes have maintenance pending"
		}
		return a
	}
	a.Stop = true
	a.Why = fmt.Sprintf("maintenance on %s is due %s", first.Name, first.Event.Deadline.UTC().Format(time.RFC3339))
	// first leaves either way, and so do the idle nodes going now.
	room := up - len(a.Trigger) - 1 - c.Size
	if first.Event.CanReschedule {
		a.Trigger = append(a.Trigger, *first)
	}
	var cands []Node
	for _, n := range s.Nodes {
		if n.Up && n.InJob && n.Name != first.Name && c.early(s.Now, &n) {
			cands = append(cands, n)
		}
	}
	a.Trigger = append(a.Trigger, ride(cands, room)...)
	sortByName(a.Trigger)
	return a
}

func down(s State, c Config) Action {
	var a Action
	var cands []Node
	up := 0
	for _, n := range s.Nodes {
		if !n.Up {
			continue
		}
		up++
		if c.early(s.Now, &n) {
			cands = append(cands, n)
		}
	}
	a.Trigger = ride(cands, up-c.Size)
	if len(a.Trigger) > 0 {
		a.Why = "job is down, so pending maintenance rides along on the restart"
	}
	sortByName(a.Trigger)
	return a
}

// ride picks which candidates go along with a restart, given room spare
// nodes beyond what the job needs. If room is negative the restart has to
// wait for nodes to come back anyway, so all of them go. Otherwise each
// one past the room would hold up the restart for a whole maintenance, so
// only the room's worth go, soonest deadline first.
func ride(cands []Node, room int) []Node {
	if room < 0 || len(cands) <= room {
		return cands
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Event.Deadline.Before(cands[j].Event.Deadline) })
	return cands[:room]
}

// early reports whether n's event can and should be started now.
func (c Config) early(now time.Time, n *Node) bool {
	return n.Event != nil && n.Event.CanReschedule && (c.Horizon == 0 || n.Event.Deadline.Sub(now) <= c.Horizon)
}

func sortByName(ns []Node) {
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
}
