// Command lockstep lines up host maintenance on GPU VMs that can't
// live-migrate, so a gang job pays for one restart instead of one per
// node.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Rakshit-gen/lockstep/ctl"
	"github.com/Rakshit-gen/lockstep/gce"
	"github.com/Rakshit-gen/lockstep/meta"
	"github.com/Rakshit-gen/lockstep/plan"
	"github.com/Rakshit-gen/lockstep/sim"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `lockstep lines up host maintenance for a gang job on GPU VMs.

  lockstep status --project p --filter 'labels.job = "llama"'
  lockstep plan   --project p --filter ... --size 64 --status-hook ./status.sh
  lockstep run    --project p --filter ... --size 64 --status-hook ... --stop-hook ... [--start-hook ...]
  lockstep agent  --hook ./checkpoint.sh
  lockstep sim    [--policy lockstep] [--size 256 --spares 16] [--preset a3]
  lockstep bench  [--seeds 20]
  lockstep version

Run a command with -h for its flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"status": statusCmd, "plan": planCmd, "run": runCmd, "agent": agentCmd, "sim": simCmd, "bench": benchCmd,
	}
	name, args := os.Args[1], os.Args[2:]
	switch name {
	case "version", "--version", "-v":
		fmt.Printf("lockstep %s (%s, %s)\n", version, commit, date)
		return
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	}
	cmd, ok := cmds[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "lockstep: no command %q\n\n%s", name, usage)
		os.Exit(2)
	}
	if err := cmd(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "lockstep:", err)
		}
		os.Exit(1)
	}
}

// cluster holds the flags every command that talks to Compute Engine
// shares.
type cluster struct {
	project, filter string
	size            int
	margin, horizon time.Duration
	status          string
}

func (c *cluster) flags(fs *flag.FlagSet, withPlan bool) {
	fs.StringVar(&c.project, "project", os.Getenv("CLOUDSDK_CORE_PROJECT"), "Google Cloud project")
	fs.StringVar(&c.filter, "filter", "", `API filter for the job's VMs, like 'labels.job = "llama"'`)
	if withPlan {
		fs.IntVar(&c.size, "size", 0, "VMs the job needs; the rest are spares")
		fs.DurationVar(&c.margin, "margin", 15*time.Minute, "stop the job this long before a deadline")
		fs.DurationVar(&c.horizon, "horizon", 14*24*time.Hour, "start maintenance early only if it is due within this")
		fs.StringVar(&c.status, "status-hook", "", `command that prints "running" and the job's VM names, or "down"`)
	}
}

func (c *cluster) check(withPlan bool) error {
	if c.project == "" {
		return fmt.Errorf("--project is required")
	}
	if c.filter == "" {
		return fmt.Errorf("--filter is required, so lockstep only touches the job's VMs")
	}
	if withPlan {
		if c.size < 1 {
			return fmt.Errorf("--size is required")
		}
		if c.status == "" {
			return fmt.Errorf("--status-hook is required")
		}
	}
	return nil
}

func (c *cluster) controller() *ctl.Controller {
	return &ctl.Controller{
		GCE:    gce.New(c.project),
		Filter: c.filter,
		Plan:   plan.Config{Size: c.size, Margin: c.margin, Horizon: c.horizon},
		Hooks:  ctl.Hooks{Status: c.status},
		Logf:   log.Printf,
	}
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var c cluster
	c.flags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.check(false); err != nil {
		return err
	}
	ins, err := gce.New(c.project).Instances(context.Background(), c.filter)
	if err != nil {
		return err
	}
	fmt.Printf("%-28s %-16s %-11s %s\n", "NAME", "ZONE", "STATUS", "MAINTENANCE")
	for _, in := range ins {
		fmt.Printf("%-28s %-16s %-11s %s\n", in.Name, in.Zone, in.Status, describe(in.Maintenance))
	}
	return nil
}

func describe(m *gce.Maintenance) string {
	if m == nil {
		return "-"
	}
	s := strings.ToLower(m.Status)
	if d := m.Deadline(); !d.IsZero() {
		s += " by " + d.UTC().Format(time.RFC3339) + " (in " + until(time.Until(d)) + ")"
	}
	if m.Emergency() {
		s += ", emergency"
	}
	if !m.CanReschedule {
		s += ", fixed"
	}
	return s
}

func until(d time.Duration) string {
	switch {
	case d < 0:
		return "the past"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func planCmd(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	var c cluster
	c.flags(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.check(true); err != nil {
		return err
	}
	k := c.controller()
	k.DryRun = true
	a, err := k.Once(context.Background())
	if err != nil {
		return err
	}
	if !a.Stop && len(a.Trigger) == 0 {
		fmt.Println("nothing to do now")
	}
	if !a.Wake.IsZero() {
		fmt.Printf("next decision at %s\n", a.Wake.UTC().Format(time.RFC3339))
	}
	return nil
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var c cluster
	c.flags(fs, true)
	stop := fs.String("stop-hook", "", "command that checkpoints and stops the job")
	start := fs.String("start-hook", "", "command that restarts the job; LOCKSTEP_NODES lists the VMs it can use")
	poll := fs.Duration("poll", time.Minute, "how often to look")
	dry := fs.Bool("dry-run", false, "log what would happen and do nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.check(true); err != nil {
		return err
	}
	if *stop == "" && !*dry {
		return fmt.Errorf("--stop-hook is required")
	}
	k := c.controller()
	k.Hooks.Stop, k.Hooks.Start, k.Poll, k.DryRun = *stop, *start, *poll, *dry
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := k.Loop(ctx); !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func agentCmd(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	hook := fs.String("hook", "", "command to run when the host is about to stop this VM; LOCKSTEP_EVENT holds the event")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *hook == "" {
		return fmt.Errorf("--hook is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	m := meta.New()
	if up, err := m.Upcoming(ctx); err != nil {
		log.Printf("upcoming-maintenance: %v", err)
	} else {
		log.Printf("upcoming maintenance: %s", describe(up))
	}
	err := m.Watch(ctx, func(ev string) {
		log.Printf("maintenance-event is %s", ev)
		if ev == "NONE" || ev == "" {
			return
		}
		x := execHook(ctx, *hook, "LOCKSTEP_EVENT="+ev)
		if err := x.Run(); err != nil {
			log.Printf("hook: %v", err)
		}
	}, log.Printf)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func simCmd(args []string) error {
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	c := sim.Defaults()
	policy := fs.String("policy", "", "none, agent, grouped, lockstep or both; all of them if empty")
	preset := fs.String("preset", "a3", "maintenance cadence: a3 (90 days, 90 days notice), a3-edge (30, 7) or gpu (14 days, 1 hour)")
	seeds := fs.Int("seeds", 20, "runs to average")
	fs.IntVar(&c.Size, "size", 64, "nodes the job needs")
	fs.IntVar(&c.Spares, "spares", 4, "spare nodes")
	fs.Float64Var(&c.Days, "days", c.Days, "days to simulate")
	fs.BoolVar(&c.Calendar, "calendar", false, "planned maintenance keeps a fixed calendar instead of counting from when it last ran")
	fs.Float64Var(&c.EmRate, "emergency-rate", c.EmRate, "emergency maintenance events per node per day (assumed)")
	fs.Float64Var(&c.FailRate, "fail-rate", c.FailRate, "failures per node per day (default from the Llama 3 paper)")
	fs.Float64Var(&c.Maint, "maint", c.Maint, "hours a node is gone for maintenance (assumed)")
	fs.Float64Var(&c.Repair, "repair", c.Repair, "hours a node is gone after a failure (assumed)")
	fs.Float64Var(&c.Restart, "restart", c.Restart, "hours to restart the job (assumed)")
	fs.Float64Var(&c.Ckpt, "ckpt", c.Ckpt, "hours between checkpoints (assumed)")
	fs.Float64Var(&c.Margin, "margin", c.Margin, "hours before a deadline lockstep stops the job")
	horizon := fs.Float64("horizon", 0, "hours ahead lockstep starts maintenance early; default fits the preset")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := sim.FindPreset(*preset)
	if err != nil {
		return err
	}
	p.Apply(&c)
	if *horizon > 0 {
		c.Horizon = *horizon
	}
	pols := []sim.Policy{sim.None, sim.Agent, sim.Grouped, sim.Lockstep, sim.Both}
	if *policy != "" {
		pol, err := sim.ParsePolicy(*policy)
		if err != nil {
			return err
		}
		pols = []sim.Policy{pol}
	}
	fmt.Printf("invented world: %d nodes + %d spares, %s preset, %.0f days, %d seeds\n", c.Size, c.Spares, p.Name, c.Days, *seeds)
	fmt.Printf("%-9s %8s %7s %8s %12s %8s %8s\n", "policy", "goodput", "stops", "crashes", "maintenance", "early", "lost h")
	for _, pol := range pols {
		c.Policy = pol
		r, err := sim.Mean(c, *seeds)
		if err != nil {
			return err
		}
		fmt.Printf("%-9s %8.4f %7d %8d %12d %8d %8.0f\n", pol, r.Goodput, r.Stops, r.Crashes, r.Maintenance, r.Early, r.Lost)
	}
	return nil
}

func benchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	seeds := fs.Int("seeds", 20, "runs to average per cell")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return sim.Bench(os.Stdout, *seeds)
}

func execHook(ctx context.Context, cmd string, env ...string) *exec.Cmd {
	x := exec.CommandContext(ctx, "sh", "-c", cmd)
	x.Env = append(os.Environ(), env...)
	x.Stdout, x.Stderr = os.Stdout, os.Stderr
	return x
}
