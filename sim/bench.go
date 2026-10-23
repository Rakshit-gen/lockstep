package sim

import (
	"fmt"
	"io"
	"sync"
)

// Mean averages a config over seeds 1..n, running them in parallel.
func Mean(c Config, n int) (Result, error) {
	if err := c.Check(); err != nil {
		return Result{}, err
	}
	rs := make([]Result, n)
	var wg sync.WaitGroup
	for i := range rs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := c
			c.Seed = uint64(i + 1)
			rs[i], _ = Run(c)
		}(i)
	}
	wg.Wait()
	var m Result
	for _, r := range rs {
		m.Goodput += r.Goodput / float64(n)
		m.Lost += r.Lost / float64(n)
		m.Down += r.Down / float64(n)
		m.Stops += r.Stops
		m.Crashes += r.Crashes
		m.Maintenance += r.Maintenance
		m.Early += r.Early
	}
	m.Stops /= n
	m.Crashes /= n
	m.Maintenance /= n
	m.Early /= n
	return m, nil
}

// Preset is a machine family's maintenance as Google documents it.
type Preset struct {
	Name           string
	Period, Notice float64 // hours
}

// Presets come from the AI Hypercomputer host events page and the GPU
// host maintenance page.
var Presets = []Preset{
	{"a3", 90 * 24, 90 * 24},     // A3 High, Mega, Ultra; A4; A4X
	{"a3-edge", 30 * 24, 7 * 24}, // A3 Edge
	{"gpu", 14 * 24, 1},          // other GPU VMs: about every two weeks, an hour's notice
}

// FindPreset looks a preset up by name.
func FindPreset(name string) (Preset, error) {
	for _, p := range Presets {
		if p.Name == name {
			return p, nil
		}
	}
	return Preset{}, fmt.Errorf("unknown preset %q (have a3, a3-edge, gpu)", name)
}

// Apply sets the preset's cadence and a horizon that fits it.
func (p Preset) Apply(c *Config) {
	c.Period, c.Notice = p.Period, p.Notice
	c.Horizon = min(14*24, p.Period/4)
}

var compared = []Policy{None, Agent, Grouped, Lockstep, Both}

// Bench writes the tables the README quotes, as markdown.
func Bench(w io.Writer, seeds int) error {
	fmt.Fprintf(w, "Every number below comes from the simulator's invented world, averaged over %d seeds of a 365 day run.\n", seeds)
	d := Defaults()
	fmt.Fprintf(w, "Assumed unless the flag is changed: restart %.1fh, maintenance %.0fh, repair %.0fh, checkpoint every %.0fh, %.0f minute final checkpoint, emergency events %.3f per node per day with %.0f days notice.\n", d.Restart, d.Maint, d.Repair, d.Ckpt, d.Write*60, d.EmRate, d.EmNotice/24)
	fmt.Fprintf(w, "Failures %.4f per node per day, from the Llama 3 paper.\n\n", d.FailRate)

	fmt.Fprintf(w, "### Goodput by job size, A3 cadence, one spare per 16 nodes\n\n")
	fmt.Fprintf(w, "| nodes | spares | none | agent | grouped | lockstep | both | stops agent | stops lockstep |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, size := range []int{16, 64, 256, 1024} {
		c := Defaults()
		c.Size, c.Spares = size, size/16
		rs, err := each(c, seeds)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "| %d | %d |", size, c.Spares)
		for _, r := range rs {
			fmt.Fprintf(w, " %.4f |", r.Goodput)
		}
		fmt.Fprintf(w, " %d | %d |\n", rs[Agent].Stops, rs[Lockstep].Stops)
	}

	fmt.Fprintf(w, "\n### Spares, 256 nodes, A3 cadence\n\n")
	fmt.Fprintf(w, "| spares | none | agent | grouped | lockstep | both |\n|---|---|---|---|---|---|\n")
	for _, sp := range []int{0, 4, 16, 64} {
		c := Defaults()
		c.Size, c.Spares = 256, sp
		rs, err := each(c, seeds)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "| %d |", sp)
		for _, r := range rs {
			fmt.Fprintf(w, " %.4f |", r.Goodput)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "\n### Machine families, 256 nodes, 16 spares\n\n")
	fmt.Fprintf(w, "| preset | period | notice | none | agent | grouped | lockstep | both |\n|---|---|---|---|---|---|---|---|\n")
	for _, p := range Presets {
		c := Defaults()
		c.Size, c.Spares = 256, 16
		p.Apply(&c)
		rs, err := each(c, seeds)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "| %s | %s | %s |", p.Name, span(p.Period), span(p.Notice))
		for _, r := range rs {
			fmt.Fprintf(w, " %.4f |", r.Goodput)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "\n### Horizon, 256 nodes, 16 spares, A3 cadence\n\n")
	fmt.Fprintf(w, "| horizon | goodput | stops | maintenance run | started early |\n|---|---|---|---|---|\n")
	for _, h := range []float64{1, 24, 3 * 24, 7 * 24, 14 * 24, 30 * 24} {
		c := Defaults()
		c.Size, c.Spares, c.Horizon = 256, 16, h
		r, err := Mean(c, seeds)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "| %s | %.4f | %d | %d | %d |\n", span(h), r.Goodput, r.Stops, r.Maintenance, r.Early)
	}
	return nil
}

func each(c Config, seeds int) ([]Result, error) {
	rs := make([]Result, len(compared))
	for i, p := range compared {
		c.Policy = p
		r, err := Mean(c, seeds)
		if err != nil {
			return nil, err
		}
		rs[i] = r
	}
	return rs, nil
}

func span(h float64) string {
	if h < 24 {
		return fmt.Sprintf("%.0fh", h)
	}
	return fmt.Sprintf("%.0fd", h/24)
}
