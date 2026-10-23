package gce_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Rakshit-gen/lockstep/gce"
	"github.com/Rakshit-gen/lockstep/gce/gcetest"
)

func client(s *gcetest.Server) *gce.Client {
	return &gce.Client{Base: s.Base(), Project: "p", HTTP: http.DefaultClient, Token: gce.StaticToken("t"), Retries: 2}
}

func TestInstancesPagesAndParses(t *testing.T) {
	s := gcetest.New()
	defer s.Close()
	s.PageSize = 2
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	for i, n := range []string{"a0", "a1", "a2", "b0"} {
		in := gcetest.Instance{Name: n, Zone: "us-central1-a", Labels: map[string]string{"job": "llama"}}
		if n[0] == 'b' {
			in.Labels["job"] = "other"
		}
		if i == 1 {
			in.Maintenance = gcetest.Pending(start, true, "FAILURE_GPU_XID")
		}
		s.Put(in)
	}
	got, err := client(s).Instances(context.Background(), `labels.job = "llama"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d instances", len(got))
	}
	var m *gce.Maintenance
	for _, in := range got {
		if in.Zone != "us-central1-a" {
			t.Errorf("zone %q", in.Zone)
		}
		if in.Name == "a1" {
			m = in.Maintenance
		} else if in.Maintenance != nil {
			t.Errorf("%s has maintenance", in.Name)
		}
	}
	if m == nil || !m.Pending() || !m.CanReschedule || !m.WindowStart.Equal(start) || !m.Emergency() {
		t.Errorf("a1 maintenance %+v", m)
	}
	if !m.Deadline().Equal(start) {
		t.Errorf("deadline %v", m.Deadline())
	}
}

func TestPerformMaintenance(t *testing.T) {
	s := gcetest.New()
	defer s.Close()
	s.Put(gcetest.Instance{Name: "n1", Zone: "z", Maintenance: gcetest.Pending(time.Now().Add(time.Hour), true)})
	s.Put(gcetest.Instance{Name: "n2", Zone: "z", Maintenance: gcetest.Pending(time.Now().Add(time.Hour), false)})
	s.Fail["n1"] = 503
	c := client(s)
	ctx := context.Background()
	op, err := c.PerformMaintenance(ctx, "z", "n1")
	if err != nil {
		t.Fatalf("retry after 503 failed: %v", err)
	}
	if err := c.Wait(ctx, op); err != nil {
		t.Fatal(err)
	}
	if in, _ := s.Get("n1"); in.Status != "TERMINATED" {
		t.Errorf("n1 is %s", in.Status)
	}
	_, err = c.PerformMaintenance(ctx, "z", "n2")
	var ae *gce.APIError
	if !errors.As(err, &ae) || ae.Code != 400 {
		t.Errorf("rescheduling a fixed window: %v", err)
	}
	if got := s.Calls(); len(got) != 1 || got[0] != "n1" {
		t.Errorf("calls %v", got)
	}
}

func TestMetadataToken(t *testing.T) {
	srv := http.NewServeMux()
	n := 0
	srv.HandleFunc("/computeMetadata/v1/instance/service-accounts/default/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(403)
			return
		}
		n++
		w.Write([]byte(`{"access_token":"ya29.x","expires_in":3599}`))
	})
	ts := httptestServer(t, srv)
	m := &gce.MetadataToken{Host: ts}
	for i := 0; i < 3; i++ {
		tok, err := m.Token(context.Background())
		if err != nil || tok != "ya29.x" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
	if n != 1 {
		t.Errorf("fetched %d times, want once", n)
	}
}
