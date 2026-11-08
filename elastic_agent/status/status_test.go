package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteAllowed(t *testing.T) {
	allowed := parseIPs(ingressProxyIP)
	cases := map[string]bool{
		"172.30.32.2:51234": true,
		"127.0.0.1:8099":    true,
		"[::1]:8099":        true,
		"172.30.32.5:51234": false,
		"192.168.1.10:1234": false,
		"garbage":           false,
	}
	for addr, want := range cases {
		if got := remoteAllowed(addr, allowed); got != want {
			t.Errorf("remoteAllowed(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestHandlerRefusesOtherAddresses(t *testing.T) {
	h := newHandler(NewCollector(Paths{}), parseIPs(ingressProxyIP))
	for addr, want := range map[string]int{"172.30.32.2:1": 200, "172.30.32.9:1": 403} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s: got HTTP %d, want %d", addr, rec.Code, want)
		}
	}
}

func TestHandlerServesPage(t *testing.T) {
	h := newHandler(NewCollector(Paths{}), nil)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s: got HTTP %d", path, rec.Code)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: no Content-Security-Policy", path)
		}
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("https://elastic:secret@es.example.com:443/"); got != "https://es.example.com:443/" {
		t.Errorf("got %q", got)
	}
	if got := redactURL("not a url"); got != "not a url" {
		t.Errorf("got %q", got)
	}
}

func TestRedactText(t *testing.T) {
	cases := map[string]string{
		"Connected at https://elastic:s3cret@es.example.com:9200.":       "Connected at https://es.example.com:9200.",
		"Can't reach http://user@10.0.0.1/ or https://a:b@c.example/x?y": "Can't reach http://10.0.0.1/ or https://c.example/x?y",
		"Get \"https://elastic:***@es:9200/\": dial tcp: refused":        "Get \"https://es:9200/\": dial tcp: refused",
		"Mail me at admin@example.com about https://es.example.com/":     "Mail me at admin@example.com about https://es.example.com/",
		"https://es.example.com/path@v1":                                 "https://es.example.com/path@v1",
	}
	for in, want := range cases {
		if got := redactText(in); got != want {
			t.Errorf("redactText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReportRedactsAgentMessages(t *testing.T) {
	es := fakeES(t, 200)
	defer es.Close()
	startup := Startup{Mode: "standalone", StartedAt: time.Now().Add(-time.Hour), ElasticsearchURL: es.URL,
		Pipeline: pipelineName, Inputs: []string{"haos-system"}}
	p := standaloneFixture(t, es.URL, startup, map[string]any{"elasticsearch_url": es.URL})
	agent := healthyAgent()
	agent.Components[0].Units[0].State = "failed"
	agent.Components[0].Units[0].Message = "can't connect to https://elastic:hunter2@es.example.com:9200"
	r := newTestCollector(p, agent).Report(context.Background(), false)
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "hunter2") {
		t.Errorf("the report contains the password: %s", b)
	}
	if !strings.Contains(checksByID(r.Checks)["components"].Detail, "https://es.example.com:9200") {
		t.Errorf("components detail lost the URL: %+v", checksByID(r.Checks)["components"])
	}
}

func TestParseAgentStatus(t *testing.T) {
	raw := `{
	  "info": {"id": "abc", "version": "9.5.6", "is_managed": false},
	  "state": 2, "message": "Running", "FleetState": 6, "FleetMessage": "Not enrolled into Fleet",
	  "components": [{
	    "id": "journald-default", "name": "journald", "state": 3, "message": "Degraded",
	    "units": [
	      {"unit_id": "journald-default", "unit_type": 1, "state": 2, "message": "Healthy", "payload": null},
	      {"unit_id": "journald-default-haos-core", "unit_type": 0, "state": 3, "message": "",
	       "payload": {"streams": {"haos-core": {"status": "DEGRADED", "error": "can't read journal"}}}}
	    ]
	  }]
	}`
	st, err := parseAgentStatus([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.State != "healthy" || st.Version != "9.5.6" || st.FleetState != "stopped" {
		t.Fatalf("unexpected status: %+v", st)
	}
	u := st.Components[0].Units[1]
	if u.Type != "input" || u.State != "degraded" || len(u.Streams) != 1 || u.Streams[0].Message != "can't read journal" {
		t.Fatalf("unexpected unit: %+v", u)
	}
	if st.Components[0].Units[0].Type != "output" {
		t.Fatalf("unit type not mapped")
	}

	checks := agentChecks(&Startup{Mode: "standalone"}, st, time.Now())
	byID := checksByID(checks)
	if byID["agent"].Status != StatusOK {
		t.Errorf("agent check: %+v", byID["agent"])
	}
	if c := byID["components"]; c.Status != StatusWarn || !strings.Contains(c.Detail, "journald-default-haos-core") {
		t.Errorf("components check: %+v", c)
	}
	if _, ok := byID["fleet"]; ok {
		t.Errorf("standalone mode shouldn't have a Fleet check")
	}
}

func TestAgentNotRunning(t *testing.T) {
	down := &AgentStatus{Running: false, Error: "no socket"}
	old := &Startup{Mode: "standalone", StartedAt: time.Now().Add(-time.Hour)}
	if c := checksByID(agentChecks(old, down, time.Now()))["agent"]; c.Status != StatusError {
		t.Errorf("standalone agent down: %+v", c)
	}
	fleet := &Startup{Mode: "fleet", StartedAt: time.Now().Add(-time.Hour)}
	byID := checksByID(agentChecks(fleet, down, time.Now()))
	if byID["agent"].Status != StatusWarn || byID["fleet"].Status != StatusWarn {
		t.Errorf("fleet agent not enrolled: %+v", byID)
	}
}

func TestOverall(t *testing.T) {
	if got := overall([]Check{{Status: StatusOK}, {Status: StatusInfo}}); got != StatusOK {
		t.Errorf("info shouldn't affect overall: %s", got)
	}
	if got := overall([]Check{{Status: StatusWarn}, {Status: StatusError}, {Status: StatusOK}}); got != StatusError {
		t.Errorf("got %s", got)
	}
}

// fakeES answers like Elasticsearch for an API key that can write the app's
// data streams.
func fakeES(t *testing.T, pipelineStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "ApiKey ") {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`{"cluster_name": "test", "version": {"number": "9.5.6", "build_flavor": "default"}}`))
		case "/_security/user/_has_privileges":
			var req struct {
				Index []struct{ Names []string } `json:"index"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			all := true
			index := map[string]map[string]bool{}
			for _, n := range req.Index[0].Names {
				ok := strings.HasPrefix(n, "logs-haos.")
				all = all && ok
				index[n] = map[string]bool{"create_doc": ok, "auto_configure": ok}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"has_all_requested": all, "index": index})
		case "/_ingest/pipeline/" + pipelineName:
			w.WriteHeader(pipelineStatus)
			if pipelineStatus == 200 {
				_, _ = w.Write([]byte(`{"logs-haos.core@custom": {"version": 1}}`))
			} else {
				_, _ = w.Write([]byte(`{}`))
			}
		default:
			w.WriteHeader(404)
		}
	}))
}

func writeFile(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func standaloneFixture(t *testing.T, esURL string, startup Startup, options map[string]any) Paths {
	t.Helper()
	dir := t.TempDir()
	p := Paths{
		Options:     filepath.Join(dir, "options.json"),
		RunDir:      dir,
		DataDir:     dir,
		JournalDirs: []string{filepath.Join(dir, "journal")},
	}
	writeFile(t, p.Options, options)
	raw, _ := os.ReadFile(p.Options)
	startup.OptionsSHA256 = sha256Hex(raw)
	writeFile(t, filepath.Join(dir, "startup.json"), startup)
	writeFile(t, filepath.Join(dir, "elastic-agent.yml"), map[string]any{
		"outputs": map[string]any{"default": map[string]any{
			"type": "elasticsearch", "hosts": []string{esURL}, "api_key": "id:secret-key",
		}},
	})
	if startup.MachineID != "" {
		_ = os.MkdirAll(filepath.Join(dir, "journal", startup.MachineID), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "journal", startup.MachineID, "system.journal"), nil, 0o644)
	}
	_ = os.WriteFile(filepath.Join(dir, "startup.log"), []byte("[ha-entrypoint] Running in standalone mode.\n"+
		"[ha-elasticsearch-setup] Connected to Elasticsearch 9.5.6 at https://elastic:hunter2@es.example.com:9200.\n"+
		"[ha-entrypoint] WARNING: something\n"), 0o600)
	return p
}

func newTestCollector(p Paths, agent *AgentStatus) *Collector {
	c := NewCollector(p)
	c.agent = func(context.Context) *AgentStatus { return agent }
	return c
}

func healthyAgent() *AgentStatus {
	return &AgentStatus{Running: true, Version: "9.5.6", State: "healthy", Message: "Running", FleetState: "stopped",
		Components: []Component{{ID: "journald-default", Name: "journald", State: "healthy",
			Units: []Unit{{ID: "journald-default", Type: "output", State: "healthy"}}}}}
}

func TestStandaloneReport(t *testing.T) {
	es := fakeES(t, 200)
	defer es.Close()
	startup := Startup{
		Mode: "standalone", StartedAt: time.Now().Add(-time.Hour), HostName: "homeassistant",
		HostNameSource: "supervisor", MachineID: "0123456789abcdef0123456789abcdef",
		ElasticsearchURL: es.URL, Pipeline: pipelineName, Credential: "created",
		Inputs: []string{"haos-core", "haos-system"},
	}
	p := standaloneFixture(t, es.URL, startup, map[string]any{
		"elasticsearch_url": es.URL, "username": "elastic", "password": "hunter2",
		"collect_apps": false,
	})
	r := newTestCollector(p, healthyAgent()).Report(context.Background(), false)

	if r.Overall != StatusOK {
		t.Errorf("overall = %s, checks: %+v", r.Overall, r.Checks)
	}
	byID := checksByID(r.Checks)
	for _, id := range []string{"agent", "components", "elasticsearch", "write_access", "pipeline", "mode", "credential", "tls", "options_changed", "host_name", "journal"} {
		if c, ok := byID[id]; !ok || c.Status != StatusOK {
			t.Errorf("%s: %+v", id, c)
		}
	}
	if !strings.Contains(byID["elasticsearch"].Summary, "9.5.6") {
		t.Errorf("elasticsearch summary: %q", byID["elasticsearch"].Summary)
	}
	if byID["password"].Status != StatusInfo {
		t.Errorf("password advice: %+v", byID["password"])
	}
	if !strings.Contains(byID["sources"].Detail, "Apps") {
		t.Errorf("sources: %+v", byID["sources"])
	}
	if len(r.StartupLog) != 3 || r.StartupLog[2].Level != "warn" {
		t.Errorf("startup log: %+v", r.StartupLog)
	} else if want := "at https://es.example.com:9200."; !strings.Contains(r.StartupLog[1].Text, want) {
		t.Errorf("startup log line 2 = %q, want it to contain %q", r.StartupLog[1].Text, want)
	}

	b, _ := json.Marshal(r)
	for _, secret := range []string{"hunter2", "secret-key"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the report contains %q", secret)
		}
	}
}

func TestStandalonePipelineProblems(t *testing.T) {
	cases := []struct {
		name           string
		pipelineStatus int
		startupPipe    string
		want           Status
	}{
		{"deleted after start", 404, pipelineName, StatusError},
		{"never installed", 404, "", StatusWarn},
		{"installed after start", 200, "", StatusWarn},
		{"can't read", 403, pipelineName, StatusInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			es := fakeES(t, tc.pipelineStatus)
			defer es.Close()
			startup := Startup{Mode: "standalone", ElasticsearchURL: es.URL, Pipeline: tc.startupPipe,
				Credential: "configured", Inputs: []string{"haos-core"}, HostNameSource: "supervisor"}
			p := standaloneFixture(t, es.URL, startup, map[string]any{"elasticsearch_url": es.URL})
			r := newTestCollector(p, healthyAgent()).Report(context.Background(), false)
			if got := checksByID(r.Checks)["pipeline"].Status; got != tc.want {
				t.Errorf("pipeline status = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestOptionsChangedAndUnreachable(t *testing.T) {
	// A closed listener, so the connection is refused.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := "http://" + ln.Addr().String()
	ln.Close()
	startup := Startup{Mode: "standalone", ElasticsearchURL: addr, Credential: "saved", Inputs: []string{"haos-system"}}
	p := standaloneFixture(t, addr, startup, map[string]any{"elasticsearch_url": addr})
	writeFile(t, p.Options, map[string]any{"elasticsearch_url": addr, "collect_host": false})

	r := newTestCollector(p, healthyAgent()).Report(context.Background(), false)
	byID := checksByID(r.Checks)
	if byID["options_changed"].Status != StatusWarn {
		t.Errorf("options_changed: %+v", byID["options_changed"])
	}
	if c := byID["elasticsearch"]; c.Status != StatusError || !strings.Contains(c.Summary, "Can't reach") {
		t.Errorf("elasticsearch: %+v", c)
	}
	if byID["host_name"].Status != StatusWarn || byID["journal"].Status != StatusError {
		t.Errorf("host checks: %+v %+v", byID["host_name"], byID["journal"])
	}
	if r.Overall != StatusError {
		t.Errorf("overall = %s", r.Overall)
	}
}

func TestFleetServerCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			_, _ = w.Write([]byte(`{"name": "fleet-server", "status": "HEALTHY"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	if c := fleetServerChecks(context.Background(), &Startup{FleetURL: srv.URL})[0]; c.Status != StatusOK {
		t.Errorf("healthy Fleet Server: %+v", c)
	}
	if c := fleetServerChecks(context.Background(), &Startup{FleetURL: "http://fleet.invalid:8220"})[0]; c.Status != StatusError {
		t.Errorf("unreachable Fleet Server: %+v", c)
	}
}

func TestMissingStartup(t *testing.T) {
	r := newTestCollector(Paths{RunDir: t.TempDir()}, nil).Report(context.Background(), false)
	if r.Overall != StatusError || r.Checks[0].ID != "startup" {
		t.Errorf("got %+v", r)
	}
}

func checksByID(checks []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range checks {
		m[c.ID] = c
	}
	return m
}
