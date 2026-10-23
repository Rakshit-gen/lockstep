package sim

import "testing"

func TestQuietWorld(t *testing.T) {
	c := Defaults()
	c.Period, c.EmRate, c.FailRate = 1e9, 0, 0
	r, err := Run(c)
	if err != nil {
		t.Fatal(err)
	}
	// Only the first restart is lost.
	want := 1 - c.Restart/(c.Days*24)
	if r.Stops != 0 || r.Maintenance != 0 || r.Goodput < want-1e-9 || r.Goodput > want+1e-9 {
		t.Fatalf("%+v, want goodput %v", r, want)
	}
}

func TestCrashLosesSinceCheckpoint(t *testing.T) {
	c := Defaults()
	c.Policy, c.Size, c.Period, c.EmRate, c.FailRate = None, 1, 1e9, 0, 0
	c.Notice = 0
	w := &world{c: c, wake: inf, nodes: []node{{up: true, planned: inf, emAt: inf, emDue: inf, failAt: inf}}}
	w.start()
	w.t = w.ready + 2.75
	w.stop(false)
	if w.r.Lost < 0.75-1e-9 || w.r.Lost > 0.75+1e-9 || w.r.Crashes != 1 {
		t.Fatalf("%+v", w.r)
	}
}

func TestLockstepBeatsAgentWithoutSpares(t *testing.T) {
	var ag, ls float64
	for s := uint64(1); s <= 5; s++ {
		c := Defaults()
		c.Seed = s
		c.Policy = Agent
		a, _ := Run(c)
		c.Policy = Lockstep
		l, err := Run(c)
		if err != nil {
			t.Fatal(err)
		}
		ag += a.Goodput
		ls += l.Goodput
		if l.Stops >= a.Stops {
			t.Errorf("seed %d: lockstep stopped %d times, agent %d", s, l.Stops, a.Stops)
		}
	}
	if ls <= ag {
		t.Errorf("lockstep goodput %v not above agent %v", ls/5, ag/5)
	}
}

func TestCheck(t *testing.T) {
	c := Defaults()
	c.Horizon = c.Period
	if _, err := Run(c); err == nil {
		t.Fatal("ran with a horizon as long as the period")
	}
}
