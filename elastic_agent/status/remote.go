package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const pipelineName = "logs-haos.core@custom"

// target is a server the app talks to, with the TLS settings the agent uses.
type target struct {
	URL      string
	Auth     string // Authorization header value
	CAPath   string
	Insecure bool
}

type response struct {
	Status int // 0 when the server couldn't be reached
	Body   []byte
	Err    error
}

func (t target) client() (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.Insecure {
		cfg.InsecureSkipVerify = true //nolint:gosec // the user turned verification off
	} else if t.CAPath != "" {
		pem, err := os.ReadFile(t.CAPath)
		if err != nil {
			return nil, fmt.Errorf("can't read the CA certificate: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s doesn't contain a PEM certificate", t.CAPath)
		}
		cfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg
	tr.Proxy = nil
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}, nil
}

func (t target) do(ctx context.Context, method, path string, body any) response {
	client, err := t.client()
	if err != nil {
		return response{Err: err}
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.URL, "/")+path, rd)
	if err != nil {
		return response{Err: err}
	}
	if t.Auth != "" {
		req.Header.Set("Authorization", t.Auth)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return response{Err: simplifyError(err)}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return response{Status: resp.StatusCode, Body: b}
}

// reason is a short explanation of a failed request.
func (r response) reason() string {
	if r.Err != nil {
		return r.Err.Error()
	}
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	detail := ""
	if json.Unmarshal(r.Body, &e) == nil && len(e.Error) > 0 {
		var obj struct{ Reason, Type string }
		if json.Unmarshal(e.Error, &obj) == nil {
			detail = obj.Reason
			if detail == "" {
				detail = obj.Type
			}
		} else {
			_ = json.Unmarshal(e.Error, &detail)
		}
	}
	if detail != "" {
		return fmt.Sprintf("HTTP %d: %s", r.Status, detail)
	}
	return fmt.Sprintf("HTTP %d", r.Status)
}

func simplifyError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// --- Elasticsearch (standalone mode) ------------------------------------------

// standaloneOutput reads the Elasticsearch output from the agent configuration
// the entrypoint wrote, so the checks use exactly what the agent uses.
func standaloneOutput(path string) (target, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return target{}, err
	}
	var cfg struct {
		Outputs map[string]map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return target{}, fmt.Errorf("can't parse the agent configuration: %w", err)
	}
	out := cfg.Outputs["default"]
	hosts, _ := out["hosts"].([]any)
	if len(hosts) == 0 {
		return target{}, errors.New("the agent configuration has no Elasticsearch host")
	}
	t := target{}
	t.URL, _ = hosts[0].(string)
	if key, _ := out["api_key"].(string); key != "" {
		t.Auth = "ApiKey " + base64.StdEncoding.EncodeToString([]byte(key))
	}
	if mode, _ := out["ssl.verification_mode"].(string); mode == "none" {
		t.Insecure = true
	}
	if cas, _ := out["ssl.certificate_authorities"].([]any); len(cas) > 0 {
		t.CAPath, _ = cas[0].(string)
	}
	return t, nil
}

// dataStreams lists the data streams the configured inputs write to.
func dataStreams(inputs []string) []string {
	var names []string
	for _, in := range inputs {
		if ds, ok := strings.CutPrefix(in, "haos-"); ok {
			names = append(names, "logs-haos."+ds+"-default")
		}
	}
	sort.Strings(names)
	return names
}

func elasticsearchChecks(ctx context.Context, p Paths, s *Startup) []Check {
	conn := Check{ID: "elasticsearch", Group: GroupConnectivity, Title: "Elasticsearch"}
	es, err := standaloneOutput(filepath.Join(p.RunDir, "elastic-agent.yml"))
	if err != nil {
		conn.Status = StatusError
		conn.Summary = "Can't read the agent's Elasticsearch settings."
		conn.Detail = err.Error()
		return []Check{conn}
	}
	where := redactURL(es.URL)

	root := es.do(ctx, http.MethodGet, "/", nil)
	switch {
	case root.Status == http.StatusOK:
		var info struct {
			ClusterName string `json:"cluster_name"`
			Version     struct {
				Number string `json:"number"`
				Flavor string `json:"build_flavor"`
			} `json:"version"`
		}
		_ = json.Unmarshal(root.Body, &info)
		conn.Status = StatusOK
		conn.Summary = "Connected to Elasticsearch " + info.Version.Number + "."
		if info.Version.Flavor == "serverless" {
			conn.Summary = "Connected to Elasticsearch Serverless."
		}
		conn.Detail = where
		if info.ClusterName != "" {
			conn.Detail = where + " (cluster " + info.ClusterName + ")"
		}
	case root.Status == http.StatusUnauthorized:
		conn.Status = StatusError
		conn.Summary = "Elasticsearch rejected the API key. It may have been deleted or invalidated."
		conn.Detail = root.reason()
		return []Check{conn}
	case root.Status == 0:
		conn.Status = StatusError
		conn.Summary = "Can't reach Elasticsearch at " + where + "."
		conn.Detail = root.reason()
		return []Check{conn}
	case root.Status == http.StatusForbidden:
		// The key may lack the monitor privilege. Fall back to authenticating.
		auth := es.do(ctx, http.MethodGet, "/_security/_authenticate", nil)
		if auth.Status != http.StatusOK {
			conn.Status = StatusError
			conn.Summary = "Elasticsearch at " + where + " didn't accept the API key."
			conn.Detail = auth.reason()
			return []Check{conn}
		}
		conn.Status = StatusOK
		conn.Summary = "Connected to Elasticsearch."
		conn.Detail = where
	default:
		conn.Status = StatusWarn
		conn.Summary = "Unexpected answer from Elasticsearch at " + where + "."
		conn.Detail = root.reason()
	}

	checks := []Check{conn}
	if streams := dataStreams(s.Inputs); len(streams) > 0 {
		checks = append(checks, writeCheck(ctx, es, streams))
	}
	if slices.Contains(s.Inputs, "haos-core") {
		checks = append(checks, pipelineCheck(ctx, es, s))
	}
	return checks
}

func writeCheck(ctx context.Context, es target, streams []string) Check {
	ch := Check{ID: "write_access", Group: GroupConnectivity, Title: "Write access"}
	resp := es.do(ctx, http.MethodPost, "/_security/user/_has_privileges", map[string]any{
		"index": []map[string]any{{"names": streams, "privileges": []string{"create_doc", "auto_configure"}}},
	})
	if resp.Status != http.StatusOK {
		ch.Status = StatusUnknown
		ch.Summary = "Couldn't check what the API key can write."
		ch.Detail = resp.reason()
		return ch
	}
	var result struct {
		HasAll bool                       `json:"has_all_requested"`
		Index  map[string]map[string]bool `json:"index"`
	}
	_ = json.Unmarshal(resp.Body, &result)
	if result.HasAll {
		ch.Status = StatusOK
		ch.Summary = "The API key can write " + joinList(streams) + "."
		return ch
	}
	var missing []string
	for _, name := range streams {
		for priv, ok := range result.Index[name] {
			if !ok {
				missing = append(missing, name+" ("+priv+")")
			}
		}
	}
	sort.Strings(missing)
	ch.Status = StatusError
	ch.Summary = "The API key can't write every data stream, so Elasticsearch rejects some logs."
	ch.Detail = "Missing: " + strings.Join(missing, ", ")
	return ch
}

func pipelineCheck(ctx context.Context, es target, s *Startup) Check {
	ch := Check{ID: "pipeline", Group: GroupConnectivity, Title: "Core log pipeline"}
	resp := es.do(ctx, http.MethodGet, "/_ingest/pipeline/"+pipelineName, nil)
	switch resp.Status {
	case http.StatusOK:
		var body map[string]struct {
			Version int `json:"version"`
		}
		_ = json.Unmarshal(resp.Body, &body)
		if s.Pipeline == "" {
			ch.Status = StatusWarn
			ch.Summary = "The " + pipelineName + " pipeline exists now, but the agent started without it, so Core logs are sent unparsed. Restart the app."
			return ch
		}
		ch.Status = StatusOK
		ch.Summary = "Core logs are parsed by " + pipelineName + "."
		if v := body[pipelineName].Version; v > 0 {
			ch.Summary = fmt.Sprintf("Core logs are parsed by %s (version %d).", pipelineName, v)
		}
	case http.StatusNotFound:
		if s.Pipeline != "" {
			ch.Status = StatusError
			ch.Summary = "The " + pipelineName + " pipeline was deleted after the app started, so Elasticsearch rejects Core logs. Restart the app to reinstall it."
			return ch
		}
		ch.Status = StatusWarn
		ch.Summary = "The " + pipelineName + " pipeline isn't installed, so Core logs are sent unparsed."
		ch.Detail = "Add a username and password, or an API key with manage_pipeline, and restart the app to install it."
	case http.StatusForbidden:
		if s.Pipeline == "" {
			ch.Status = StatusWarn
			ch.Summary = "Core logs are sent unparsed. The API key can't read ingest pipelines."
			return ch
		}
		ch.Status = StatusInfo
		ch.Summary = "The API key can't read ingest pipelines, so the pipeline can't be checked."
	default:
		ch.Status = StatusUnknown
		ch.Summary = "Couldn't check the " + pipelineName + " pipeline."
		ch.Detail = resp.reason()
	}
	return ch
}

// --- Fleet Server (Fleet mode) ---------------------------------------------------

func fleetServerChecks(ctx context.Context, s *Startup) []Check {
	ch := Check{ID: "fleet_server", Group: GroupConnectivity, Title: "Fleet Server"}
	where := redactURL(s.FleetURL)
	fleet := target{URL: s.FleetURL, CAPath: s.CAPath, Insecure: s.Insecure}
	resp := fleet.do(ctx, http.MethodGet, "/api/status", nil)
	if resp.Status == 0 {
		ch.Status = StatusError
		ch.Summary = "Can't reach Fleet Server at " + where + "."
		ch.Detail = resp.reason()
		return []Check{ch}
	}
	var body struct {
		Status  string `json:"status"`
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	_ = json.Unmarshal(resp.Body, &body)
	switch {
	case resp.Status == http.StatusOK && strings.EqualFold(body.Status, "HEALTHY"):
		ch.Status = StatusOK
		ch.Summary = "Fleet Server is healthy."
		ch.Detail = where
	case body.Status != "":
		ch.Status = StatusWarn
		ch.Summary = "Fleet Server reports " + strings.ToLower(body.Status) + "."
		ch.Detail = where
	default:
		ch.Status = StatusWarn
		ch.Summary = "Unexpected answer from " + where + ". Is this a Fleet Server URL?"
		ch.Detail = resp.reason()
	}
	return []Check{ch}
}

// --- Supervisor ---------------------------------------------------------------

type appInfo struct {
	Version         string `json:"version"`
	VersionLatest   string `json:"version_latest"`
	UpdateAvailable bool   `json:"update_available"`
}

func querySupervisor(ctx context.Context, base, token string) *appInfo {
	if token == "" {
		return nil
	}
	sup := target{URL: base, Auth: "Bearer " + token}
	resp := sup.do(ctx, http.MethodGet, "/addons/self/info", nil)
	if resp.Status != http.StatusOK {
		return nil
	}
	var body struct {
		Data appInfo `json:"data"`
	}
	if json.Unmarshal(resp.Body, &body) != nil || body.Data.Version == "" {
		return nil
	}
	return &body.Data
}
