package ctl

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rakshit-gen/lockstep/gce"
	"github.com/Rakshit-gen/lockstep/gce/gcetest"
	"github.com/Rakshit-gen/lockstep/plan"
)

var now = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T, status string) (*gcetest.Server, *Controller, string) {
	s := gcetest.New()
	t.Cleanup(s.Close)
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	c := &Controller{
		GCE:    &gce.Client{Base: s.Base(), Project: "p", HTTP: http.DefaultClient, Token: gce.StaticToken("t")},
		Filter: `labels.job = "llama"`,
		Plan:   plan.Config{Size: 2, Margin: 15 * time.Minute, Horizon: 14 * 24 * time.Hour},
		Hooks: Hooks{
			Status: "printf '" + status + "'",
			Stop:   "echo stop >> " + log,
			Start:  "echo start $LOCKSTEP_NODES >> " + log,
		},
		Logf: t.Logf,
		Now:  func() time.Time { return now },
	}
	job := map[string]string{"job": "llama"}
	s.Put(gcetest.Instance{Name: "a", Zone: "z", Labels: job, Maintenance: gcetest.Pending(now.Add(10*time.Minute), true)})
	s.Put(gcetest.Instance{Name: "b", Zone: "z", Labels: job, Maintenance: gcetest.Pending(now.Add(3*24*time.Hour), true)})
	s.Put(gcetest.Instance{Name: "c", Zone: "z", Labels: job})
	s.Put(gcetest.Instance{Name: "d", Zone: "z", Labels: job})
	s.Put(gcetest.Instance{Name: "x", Zone: "z", Labels: map[string]string{"job": "other"}, Maintenance: gcetest.Pending(now, true)})
	return s, c, log
}

func read(t *testing.T, p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func TestStopBatchRestart(t *testing.T) {
	s, c, log := setup(t, `running\na\nb\n`)
	a, err := c.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !a.Stop {
		t.Fatalf("%+v", a)
	}
	// a is due; c and d are spares, so b rides along too.
	if got := strings.Join(s.Calls(), ","); got != "a,b" {
		t.Errorf("maintenance on %s", got)
	}
	if got := read(t, log); got != "stop\nstart c d" {
		t.Errorf("hooks ran %q", got)
	}
}

func TestDryRun(t *testing.T) {
	s, c, log := setup(t, `running\na\nb\n`)
	c.DryRun = true
	if _, err := c.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.Calls()) != 0 || read(t, log) != "" {
		t.Errorf("dry run acted: %v %q", s.Calls(), read(t, log))
	}
}

func TestBadStatusDoesNothing(t *testing.T) {
	s, c, log := setup(t, `oops`)
	if _, err := c.Once(context.Background()); err == nil {
		t.Fatal("no error for a bad status line")
	}
	if len(s.Calls()) != 0 || read(t, log) != "" {
		t.Errorf("acted on a bad status: %v %q", s.Calls(), read(t, log))
	}
}

func TestStopFailsNoMaintenance(t *testing.T) {
	s, c, _ := setup(t, `running\na\nb\n`)
	c.Hooks.Stop = "exit 1"
	if _, err := c.Once(context.Background()); err == nil {
		t.Fatal("no error when the stop hook failed")
	}
	if len(s.Calls()) != 0 {
		t.Errorf("maintenance started under a running job: %v", s.Calls())
	}
}

func TestDownRidesAlong(t *testing.T) {
	s, c, log := setup(t, `down`)
	if _, err := c.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Four up, two needed: both pending events fit in the room.
	if got := strings.Join(s.Calls(), ","); got != "a,b" {
		t.Errorf("maintenance on %s", got)
	}
	if read(t, log) != "" {
		t.Errorf("hooks ran while the job was down: %q", read(t, log))
	}
}
