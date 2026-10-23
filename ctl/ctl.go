// Package ctl is the controller: it reads a job's VMs from Compute
// Engine, asks the job's scheduler what is running through a hook, lets
// plan decide, and carries the decision out.
package ctl

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Rakshit-gen/lockstep/gce"
	"github.com/Rakshit-gen/lockstep/plan"
)

// Hooks are shell commands that connect lockstep to the job.
type Hooks struct {
	// Status prints "running" followed by the names of the VMs the job
	// is on, one per line, or "down". Anything else is an error, and
	// lockstep does nothing that round.
	Status string
	// Stop checkpoints the job and stops it. lockstep starts maintenance
	// on the job's VMs only after it exits 0.
	Stop string
	// Start, if set, restarts the job after a stop. LOCKSTEP_NODES holds
	// the VMs that are up and not going into maintenance.
	Start string
}

// Controller runs the loop for one job.
type Controller struct {
	GCE    *gce.Client
	Filter string // API filter for the job's VMs, like labels.job = "llama"
	Plan   plan.Config
	Hooks  Hooks
	DryRun bool
	Poll   time.Duration
	Logf   func(string, ...any)
	Now    func() time.Time
}

// Once runs one round and returns what it decided.
func (c *Controller) Once(ctx context.Context) (plan.Action, error) {
	ins, err := c.GCE.Instances(ctx, c.Filter)
	if err != nil {
		return plan.Action{}, err
	}
	if len(ins) == 0 {
		return plan.Action{}, fmt.Errorf("no instances match %s", c.Filter)
	}
	running, inJob, err := c.status(ctx)
	if err != nil {
		return plan.Action{}, err
	}
	s := State(c.now(), running, inJob, ins)
	a := plan.Decide(s, c.Plan)
	if a.Why != "" {
		c.Logf("%s", a.Why)
	}
	if c.DryRun {
		if a.Stop {
			c.Logf("dry run: would stop the job")
		}
		for _, n := range a.Trigger {
			c.Logf("dry run: would start maintenance on %s/%s", n.Zone, n.Name)
		}
		return a, nil
	}
	if a.Stop {
		if _, err := c.hook(ctx, c.Hooks.Stop, nil); err != nil {
			return a, fmt.Errorf("stop hook: %w", err)
		}
		c.Logf("job stopped")
	}
	going := map[string]bool{}
	var errs []string
	for _, n := range a.Trigger {
		op, err := c.GCE.PerformMaintenance(ctx, n.Zone, n.Name)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", n.Name, err))
			continue
		}
		going[n.Name] = true
		c.Logf("started maintenance on %s/%s (%s)", n.Zone, n.Name, op.Name)
	}
	if a.Stop && c.Hooks.Start != "" {
		var up []string
		for _, n := range s.Nodes {
			if n.Up && !going[n.Name] {
				up = append(up, n.Name)
			}
		}
		if len(up) < c.Plan.Size {
			c.Logf("only %d of %d VMs are up; not restarting yet", len(up), c.Plan.Size)
		} else if _, err := c.hook(ctx, c.Hooks.Start, []string{"LOCKSTEP_NODES=" + strings.Join(up, " ")}); err != nil {
			errs = append(errs, "start hook: "+err.Error())
		} else {
			c.Logf("job restarted")
		}
	}
	if len(errs) > 0 {
		return a, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return a, nil
}

// Loop runs rounds until ctx ends, sleeping for the poll interval or
// until the plan's wake time, whichever is sooner.
func (c *Controller) Loop(ctx context.Context) error {
	for {
		a, err := c.Once(ctx)
		if err != nil {
			c.Logf("%v", err)
		}
		d := c.Poll
		if !a.Wake.IsZero() {
			d = min(d, max(a.Wake.Sub(c.now()), time.Second))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
}

// State turns instances into what plan needs.
func State(now time.Time, running bool, inJob map[string]bool, ins []gce.Instance) plan.State {
	s := plan.State{Now: now, Running: running}
	for _, in := range ins {
		m := in.Maintenance
		n := plan.Node{Name: in.Name, Zone: in.Zone, InJob: running && inJob[in.Name]}
		n.Up = in.Running() && (m == nil || m.Status != "ONGOING")
		if m.Pending() && !m.Deadline().IsZero() {
			n.Event = &plan.Event{Deadline: m.Deadline(), CanReschedule: m.CanReschedule, Emergency: m.Emergency()}
		}
		s.Nodes = append(s.Nodes, n)
	}
	return s
}

func (c *Controller) status(ctx context.Context) (bool, map[string]bool, error) {
	out, err := c.hook(ctx, c.Hooks.Status, nil)
	if err != nil {
		return false, nil, fmt.Errorf("status hook: %w", err)
	}
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return false, nil, fmt.Errorf("status hook printed nothing")
	}
	switch lines[0] {
	case "down":
		return false, nil, nil
	case "running":
		in := map[string]bool{}
		for _, n := range lines[1:] {
			in[n] = true
		}
		return true, in, nil
	}
	return false, nil, fmt.Errorf("status hook printed %q, want running or down", lines[0])
}

func (c *Controller) hook(ctx context.Context, cmd string, env []string) (string, error) {
	if cmd == "" {
		return "", fmt.Errorf("no command given")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	x := exec.CommandContext(ctx, "sh", "-c", cmd)
	x.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	x.Stdout, x.Stderr = &out, &errb
	if err := x.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
