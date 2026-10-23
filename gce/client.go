package gce

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultBase is the Compute Engine v1 endpoint.
const DefaultBase = "https://compute.googleapis.com/compute/v1"

// Client calls Compute Engine for one project.
type Client struct {
	Base    string
	Project string
	HTTP    *http.Client
	Token   TokenSource
	// Retries is how many times a 429 or 5xx is tried again.
	Retries int
}

// New makes a client with the default endpoint and token source.
func New(project string) *Client {
	return &Client{Base: DefaultBase, Project: project, HTTP: &http.Client{Timeout: 3 * time.Minute}, Token: DefaultToken(), Retries: 3}
}

// APIError is an error the API returned.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("compute: %d %s", e.Code, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, q url.Values, out any) error {
	u := strings.TrimRight(c.Base, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var last error
	for try := 0; try <= c.Retries; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(1<<try) * 250 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return err
		}
		if c.Token != nil {
			tok, err := c.Token.Token(ctx)
			if err != nil {
				return fmt.Errorf("compute: token: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if method == http.MethodPost {
			req.Body = io.NopCloser(bytes.NewReader(nil))
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode >= 300 {
			var e struct {
				Error struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(body, &e)
			if e.Error.Message == "" {
				e.Error.Message = strings.TrimSpace(string(body))
			}
			last = &APIError{Code: resp.StatusCode, Message: e.Error.Message}
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
				continue
			}
			return last
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(body, out)
	}
	return last
}

// Instances lists the project's instances in every zone that match an
// API filter such as `labels.job = "llama"`.
func (c *Client) Instances(ctx context.Context, filter string) ([]Instance, error) {
	var out []Instance
	q := url.Values{}
	if filter != "" {
		q.Set("filter", filter)
	}
	q.Set("maxResults", "500")
	for {
		var page struct {
			Items map[string]struct {
				Instances []instanceJSON `json:"instances"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := c.do(ctx, http.MethodGet, "/projects/"+c.Project+"/aggregated/instances", q, &page); err != nil {
			return nil, err
		}
		for _, scope := range page.Items {
			for _, j := range scope.Instances {
				in, err := j.instance()
				if err != nil {
					return nil, err
				}
				out = append(out, in)
			}
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		q.Set("pageToken", page.NextPageToken)
	}
}

// Operation is a zonal long-running operation.
type Operation struct {
	Name   string `json:"name"`
	Zone   string `json:"zone"`
	Status string `json:"status"`
	Error  *struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

// Err is the operation's failure, if it failed.
func (o *Operation) Err() error {
	if o.Error == nil || len(o.Error.Errors) == 0 {
		return nil
	}
	var msgs []string
	for _, e := range o.Error.Errors {
		msgs = append(msgs, e.Code+": "+e.Message)
	}
	return errors.New("compute: operation " + o.Name + ": " + strings.Join(msgs, "; "))
}

// PerformMaintenance asks Compute Engine to start an instance's pending
// maintenance now instead of at its scheduled time.
func (c *Client) PerformMaintenance(ctx context.Context, zone, name string) (*Operation, error) {
	var op Operation
	err := c.do(ctx, http.MethodPost, "/projects/"+c.Project+"/zones/"+zone+"/instances/"+name+"/performMaintenance", nil, &op)
	if err != nil {
		return nil, err
	}
	op.Zone = lastPart(op.Zone)
	if op.Zone == "" {
		op.Zone = zone
	}
	return &op, nil
}

// Wait blocks until the operation is done or ctx ends.
func (c *Client) Wait(ctx context.Context, op *Operation) error {
	for op.Status != "DONE" {
		var next Operation
		// The wait method returns after the operation is done or about
		// two minutes, whichever is first.
		if err := c.do(ctx, http.MethodPost, "/projects/"+c.Project+"/zones/"+op.Zone+"/operations/"+op.Name+"/wait", nil, &next); err != nil {
			return err
		}
		next.Zone = op.Zone
		*op = next
	}
	return op.Err()
}

// TokenSource gives OAuth access tokens.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a token someone already has.
type StaticToken string

// Token returns the token.
func (s StaticToken) Token(context.Context) (string, error) { return string(s), nil }

// MetadataToken gets the instance service account's token from the
// metadata server, which is how lockstep authenticates when it runs on a
// VM.
type MetadataToken struct {
	Host string // defaults to metadata.google.internal

	mu  sync.Mutex
	tok string
	exp time.Time
}

// Token returns a cached token or fetches a new one.
func (m *MetadataToken) Token(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tok != "" && time.Until(m.exp) > time.Minute {
		return m.tok, nil
	}
	host := m.Host
	if host == "" {
		host = "metadata.google.internal"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata token: %s", resp.Status)
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	m.tok, m.exp = t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn)*time.Second)
	return m.tok, nil
}

// GcloudToken runs `gcloud auth print-access-token`, for running lockstep
// from a workstation. Tokens are kept for 30 minutes; gcloud's last an
// hour.
type GcloudToken struct {
	mu  sync.Mutex
	tok string
	at  time.Time
}

// Token returns a cached token or asks gcloud for one.
func (g *GcloudToken) Token(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.tok != "" && time.Since(g.at) < 30*time.Minute {
		return g.tok, nil
	}
	out, err := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token").Output()
	if err != nil {
		return "", fmt.Errorf("gcloud auth print-access-token: %w", err)
	}
	g.tok, g.at = strings.TrimSpace(string(out)), time.Now()
	return g.tok, nil
}

// DefaultToken picks a source: LOCKSTEP_TOKEN if set, the metadata
// server on a VM (or wherever GCE_METADATA_HOST points), gcloud
// otherwise.
func DefaultToken() TokenSource {
	if t := os.Getenv("LOCKSTEP_TOKEN"); t != "" {
		return StaticToken(t)
	}
	if h := os.Getenv("GCE_METADATA_HOST"); h != "" {
		return &MetadataToken{Host: h}
	}
	if onGCE() {
		return &MetadataToken{}
	}
	return &GcloudToken{}
}

func onGCE() bool {
	// Compute Engine VMs say so in their DMI product name.
	b, err := os.ReadFile("/sys/class/dmi/id/product_name")
	return err == nil && strings.Contains(string(b), "Google Compute Engine")
}
