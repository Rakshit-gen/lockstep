// Package sim runs a gang training job on GPU VMs that can't live-migrate,
// through a year of host maintenance and failures, under different ways
// of handling the maintenance. The world is invented: maintenance cadence
// and notice come from Google's docs, the failure rate from the Llama 3
// paper, and every other number (restart cost, maintenance length,
// repair time, checkpoint interval) is an assumption with a flag.
package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/Rakshit-gen/lockstep/plan"
)

// Policy is how maintenance is handled.
type Policy int

const (
	// None ignores maintenance: when a node is taken, the job crashes and
	// loses the work since its last checkpoint.
	None Policy = iota
	// Agent runs an agent on each node that checkpoints and stops the job
	// cleanly when its node is about to be taken. Every event still costs
	// its own restart.
	Agent
	// Grouped is the agent plus maintenance that Compute Engine runs on
	// the whole group at once, like grouped reservation maintenance:
	// planned events land on every node together.
	Grouped
	// Lockstep is the agent plus the plan package: batch pending events
	// into one stop, and start them early when the job is down anyway.
	Lockstep
	// Both is grouped maintenance with lockstep on top, for the events
	// grouping doesn't line up: emergencies, and planned work that a
	// failure can carry.
	Both
)

var policyNames = []string{"none", "agent", "grouped", "lockstep", "both"}

func (p Policy) grouped() bool { return p == Grouped || p == Both }

func (p Policy) plans() bool { return p == Lockstep || p == Both }

func (p Policy) String() string { return policyNames[p] }

// ParsePolicy reads a policy name.
func ParsePolicy(s string) (Policy, error) {
	for i, n := range policyNames {
		if n == s {
			return Policy(i), nil
		}
	}
	return 0, fmt.Errorf("unknown policy %q (have none, agent, grouped, lockstep, both)", s)
}

// Config describes the world and the job. Times are in hours, rates per
// node per day.
type Config struct {
	Policy Policy
	Size   int // nodes the job needs
	Spares int
	Days   float64

	Period   float64 // planned maintenance comes this often per node
	Notice   float64 // and is visible this long before its window
	Calendar bool    // next planned window counts from the schedule, not from when maintenance ran

	EmRate   float64 // emergency maintenance events
	EmNotice float64
	FailRate float64 // unexpected node failures

	Maint   float64 // how long a node is gone for maintenance
	Repair  float64 // and after a failure
	Restart float64 // job restart, from enough nodes up to training
	Ckpt    float64 // checkpoint interval
	Write   float64 // cost of the extra checkpoint before a clean stop

	Margin  float64 // lockstep stops this long before a deadline; must cover the final checkpoint
	Horizon float64 // see plan.Config; must be set, and shorter than Period

	Seed uint64
}

// Defaults is an A3 or A4 cluster as the AI Hypercomputer docs describe
// it, with the job numbers assumed.
func Defaults() Config {
	return Config{
		Policy: Lockstep, Size: 64, Spares: 0, Days: 365,
		Period: 90 * 24, Notice: 90 * 24,
		EmRate: 0.001, EmNotice: 7 * 24,
		// 419 unexpected interruptions in 54 days on 16,384 GPUs, or
		// 2,048 eight GPU nodes.
		FailRate: 419.0 / 54 / 2048,
		Maint:    2, Repair: 4, Restart: 0.5, Ckpt: 1, Write: 5.0 / 60,
		Margin: 0.25, Horizon: 14 * 24, Seed: 1,
	}
}

// Result is what a run measured.
type Result struct {
	Goodput     float64 // useful training time over wall time
	Stops       int     // times the job went down
	Crashes     int     // of those, without a clean checkpoint
	Maintenance int     // maintenance events run, on any node
	Early       int     // of those, started early by lockstep
	Lost        float64 // hours of training thrown away
	Down        float64 // hours the job wasn't training
}

type node struct {
	up      bool
	until   float64 // when a down node comes back
	inJob   bool
	planned float64 // next planned window
	emAt    float64 // next emergency event appears
	emDue   float64 // visible emergency deadline, +Inf if none
	failAt  float64
}

type world struct {
	c     Config
	rng   *rand.Rand
	nodes []node
	t     float64
	// The job is training when running and t >= ready.
	running  bool
	ready    float64
	runStart float64
	r        Result
	wake     float64
}

const inf = math.MaxFloat64

func (w *world) exp(perDay float64) float64 {
	if perDay <= 0 {
		return inf
	}
	return w.t + w.rng.ExpFloat64()/perDay*24
}

// Check reports a config the simulator can't run.
func (c Config) Check() error {
	switch {
	case c.Size < 1 || c.Spares < 0:
		return fmt.Errorf("sim: need at least one node and no negative spares")
	case c.Days <= 0 || c.Period <= 0 || c.Ckpt <= 0:
		return fmt.Errorf("sim: days, period and checkpoint interval must be positive")
	case c.Policy.plans() && (c.Horizon <= 0 || c.Horizon >= c.Period-c.Maint):
		// With notice as long as the period, a node shows its next event
		// as soon as maintenance ends; an open horizon would maintain it
		// again at once, forever.
		return fmt.Errorf("sim: horizon must be positive and shorter than the period less maintenance")
	}
	return nil
}

// Run simulates one job.
func Run(c Config) (Result, error) {
	if err := c.Check(); err != nil {
		return Result{}, err
	}
	w := &world{c: c, rng: rand.New(rand.NewPCG(c.Seed, c.Seed^0x9e3779b97f4a7c15)), wake: inf}
	phase := w.rng.Float64() * c.Period
	w.nodes = make([]node, c.Size+c.Spares)
	for i := range w.nodes {
		n := &w.nodes[i]
		n.up, n.emDue = true, inf
		n.planned = w.rng.Float64() * c.Period
		if c.Policy.grouped() {
			n.planned = phase
		}
		n.emAt, n.failAt = w.exp(c.EmRate), w.exp(c.FailRate)
	}
	end := c.Days * 24
	w.start()
	for w.t < end {
		next := math.Min(end, w.next())
		if w.running && next > w.ready {
			w.r.Goodput += next - math.Max(w.t, w.ready)
		}
		w.t = next
		w.step()
	}
	w.r.Goodput -= w.r.Lost
	w.r.Down = end - w.r.Goodput - w.r.Lost
	w.r.Goodput /= end
	return w.r, nil
}

// visible is a node's earliest visible deadline, or inf.
func (w *world) visible(n *node) float64 {
	d := n.emDue
	if n.planned-w.c.Notice <= w.t {
		d = math.Min(d, n.planned)
	}
	return d
}

func (w *world) next() float64 {
	t := w.wake
	if w.running && w.ready > w.t {
		t = math.Min(t, w.ready)
	}
	for i := range w.nodes {
		n := &w.nodes[i]
		if !n.up {
			t = math.Min(t, n.until)
		}
		t = math.Min(t, math.Min(n.failAt, n.emAt))
		t = math.Min(t, math.Min(n.planned, n.emDue))
		if v := n.planned - w.c.Notice; v > w.t {
			t = math.Min(t, v)
		}
	}
	return t
}

func (w *world) step() {
	c := w.c
	const eps = 1e-9
	for i := range w.nodes {
		n := &w.nodes[i]
		if !n.up && n.until <= w.t+eps {
			n.up = true
		}
		if n.emAt <= w.t+eps {
			n.emDue = math.Min(n.emDue, w.t+c.EmNotice)
			n.emAt = w.exp(c.EmRate)
		}
		if n.failAt <= w.t+eps {
			n.failAt = w.exp(c.FailRate)
			if n.up {
				w.take(n, c.Repair, false)
			}
		}
		if math.Min(n.planned, n.emDue) <= w.t+eps {
			w.maintain(n, false)
		}
	}
	if w.wake <= w.t+eps {
		w.wake = inf
	}
	if c.Policy.plans() {
		w.decide()
	}
	if !w.running {
		w.start()
	}
}

// take removes a node for d hours. A clean take is one the job knew was
// coming.
func (w *world) take(n *node, d float64, clean bool) {
	if n.up {
		n.up, n.until = false, w.t+d
	} else {
		n.until = math.Max(n.until, w.t+d)
	}
	if n.inJob && w.running {
		w.stop(clean)
	}
}

// maintain runs maintenance on a node now, clearing whatever was visible.
func (w *world) maintain(n *node, early bool) {
	c := w.c
	w.r.Maintenance++
	if early {
		w.r.Early++
	}
	if n.planned-c.Notice <= w.t+1e-9 {
		if c.Calendar {
			n.planned += c.Period
		} else {
			n.planned = w.t + c.Maint + c.Period
		}
	}
	n.emDue = inf
	// Any policy but none knows an hour ahead that its node is going.
	w.take(n, c.Maint, c.Policy != None)
}

func (w *world) stop(clean bool) {
	w.running = false
	w.r.Stops++
	if w.t < w.ready {
		// Still restarting: nothing trained, nothing lost.
	} else if clean {
		w.r.Lost += w.c.Write
	} else {
		w.r.Crashes++
		since := w.t - w.runStart
		w.r.Lost += since - math.Floor(since/w.c.Ckpt)*w.c.Ckpt
	}
	for i := range w.nodes {
		w.nodes[i].inJob = false
	}
}

// start puts the job on the up nodes whose maintenance is furthest off,
// if there are enough of them.
func (w *world) start() {
	var up []int
	for i := range w.nodes {
		if w.nodes[i].up {
			up = append(up, i)
		}
	}
	if len(up) < w.c.Size {
		return
	}
	sort.SliceStable(up, func(a, b int) bool {
		return w.visible(&w.nodes[up[a]]) > w.visible(&w.nodes[up[b]])
	})
	for _, i := range up[:w.c.Size] {
		w.nodes[i].inJob = true
	}
	w.running = true
	w.ready = w.t + w.c.Restart
	w.runStart = w.ready
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func clock(h float64) time.Time { return epoch.Add(time.Duration(h * float64(time.Hour))) }

func hours(d float64) time.Duration { return time.Duration(d * float64(time.Hour)) }

func (w *world) decide() {
	s := plan.State{Now: clock(w.t), Running: w.running}
	for i := range w.nodes {
		n := &w.nodes[i]
		pn := plan.Node{Name: fmt.Sprintf("n%05d", i), Up: n.up, InJob: n.inJob}
		if d := w.visible(n); d < inf {
			pn.Event = &plan.Event{Deadline: clock(d), CanReschedule: true}
		}
		s.Nodes = append(s.Nodes, pn)
	}
	a := plan.Decide(s, plan.Config{Size: w.c.Size, Margin: hours(w.c.Margin), Horizon: hours(w.c.Horizon)})
	if a.Stop && w.running {
		w.stop(true)
	}
	for _, n := range a.Trigger {
		var i int
		fmt.Sscanf(n.Name, "n%d", &i)
		w.maintain(&w.nodes[i], true)
	}
	w.wake = inf
	if !a.Wake.IsZero() {
		// Hours to time and back can round a nanosecond early, which would
		// wake again at the same instant forever.
		w.wake = math.Max(a.Wake.Sub(epoch).Hours(), w.t+1e-6)
	}
}
