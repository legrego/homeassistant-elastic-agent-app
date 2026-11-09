package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigurationSecurityAndPersistence(t *testing.T) {
	options := map[string]any{"elasticsearch_url": "https://es.example", "api_key": "never-return-me", "password": "!secret es_password", "username": "elastic", "collect_core": true, "tags": []any{"ha"}, "future_option": "preserved"}
	saves, restarts := 0, 0
	reject := false
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer supervisor-secret" {
			t.Error("missing server-side token")
		}
		switch r.URL.Path {
		case "/addons/self/info":
			writeConfigJSON(w, map[string]any{"result": "ok", "data": map[string]any{"options": options}})
		case "/addons/self/options":
			if reject {
				http.Error(w, "secret upstream error", 500)
				return
			}
			var b struct {
				Options map[string]any `json:"options"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			options = b.Options
			saves++
			writeConfigJSON(w, map[string]any{"result": "ok"})
		case "/addons/self/restart":
			restarts++
			writeConfigJSON(w, map[string]any{"result": "ok"})
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer supervisor.Close()
	api := newConfigAPI(Paths{SupervisorURL: supervisor.URL, SupervisorToken: "supervisor-secret"})
	mux := http.NewServeMux()
	api.register(mux)
	request := func(method, path, body string, modify func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = ingressProxyIP + ":1234"
		r.Header.Set("X-Remote-User-Id", "admin-id")
		r.Header.Set("X-Remote-User-Groups", "system-admin")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", api.sign("csrf:admin-id"))
		if modify != nil {
			modify(r)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	edit := func(values map[string]any) string {
		b, _ := json.Marshal(configEdit{Options: values, Revision: api.revision(options)})
		return string(b)
	}
	w := request("GET", "/api/config", "", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "never-return-me") || strings.Contains(w.Body.String(), "es_password") || strings.Contains(w.Body.String(), "supervisor-secret") || strings.Contains(w.Body.String(), "future_option") {
		t.Fatalf("unsafe config response: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), `"api_key":true`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing secret indicator or no-store")
	}
	for name, mutate := range map[string]func(*http.Request){
		"LAN spoof":        func(r *http.Request) { r.RemoteAddr = "192.168.1.1:1" },
		"loopback spoof":   func(r *http.Request) { r.RemoteAddr = "127.0.0.1:1" },
		"missing identity": func(r *http.Request) { r.Header.Del("X-Remote-User-Id") },
		"non-admin":        func(r *http.Request) { r.Header.Set("X-Remote-User-Groups", "system-users") },
		"missing groups":   func(r *http.Request) { r.Header.Del("X-Remote-User-Groups") },
		"substring group":  func(r *http.Request) { r.Header.Set("X-Remote-User-Groups", "not-system-admin") },
		"no CSRF":          func(r *http.Request) { r.Header.Del("X-CSRF-Token") },
		"other user CSRF":  func(r *http.Request) { r.Header.Set("X-Remote-User-Id", "another-admin") },
		"cross-site":       func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		t.Run(name, func(t *testing.T) {
			if got := request("POST", "/api/config", edit(map[string]any{"tags": []any{"new"}}), mutate); got.Code != 403 {
				t.Fatalf("got %d", got.Code)
			}
		})
	}
	if saves != 0 {
		t.Fatal("unauthorized save")
	}
	for _, body := range []string{`{}`, `{"options":{"unknown":true}}`, edit(map[string]any{"collect_core": "true"}), edit(map[string]any{"api_key": nil}), edit(map[string]any{"tags": []any{1}}), edit(map[string]any{"tags": []any{}}) + ` {}`} {
		if got := request("POST", "/api/config", body, nil); got.Code < 400 {
			t.Fatalf("accepted bad input %s", body)
		}
	}
	stale := edit(map[string]any{"tags": []any{"stale"}})
	w = request("POST", "/api/config", edit(map[string]any{"tags": []any{"new"}}), nil)
	if w.Code != 200 || saves != 1 || options["api_key"] != "never-return-me" || options["password"] != "!secret es_password" || options["future_option"] != "preserved" {
		t.Fatalf("save lost existing options: %d", w.Code)
	}
	if got := request("POST", "/api/config", stale, nil); got.Code != 409 {
		t.Fatalf("stale save: %d", got.Code)
	}
	w = request("POST", "/api/config", edit(map[string]any{"password": "", "username": "", "api_key": "replacement"}), nil)
	if w.Code != 200 || options["password"] != "" || options["api_key"] != "replacement" {
		t.Fatal("replace/clear failed")
	}
	reject = true
	w = request("POST", "/api/config", edit(map[string]any{"api_key": "new-secret"}), nil)
	if w.Code != 502 || strings.Contains(w.Body.String(), "secret upstream") || restarts != 0 {
		t.Fatal("failure leaked or restarted")
	}
	reject = false
	w = request("POST", "/api/restart", "{}", nil)
	if w.Code != 200 || restarts != 1 {
		t.Fatal("restart failed")
	}
	for _, mode := range []string{"standalone", "fleet"} {
		for _, savedURL := range []string{"!secret destination_url", "https://user:hidden-password@example.com", "https://example.com/path?key=hidden-query#hidden-fragment"} {
			t.Run(mode+" preserves "+savedURL, func(t *testing.T) {
				key := "elasticsearch_url"
				options = map[string]any{"tags": []any{}}
				if mode == "fleet" {
					key = "fleet_url"
					options["enrollment_token"] = "saved-token"
				}
				options[key] = savedURL
				w := request("GET", "/api/config", "", nil)
				var view struct {
					Options        map[string]any
					Mode           string
					ConfiguredURLs map[string]bool `json:"configured_urls"`
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
					t.Fatal("cannot load form")
				}
				if view.Mode != mode || !view.ConfiguredURLs[key] {
					t.Fatal("lost configured URL or mode")
				}
				if strings.HasPrefix(savedURL, "!secret ") {
					if view.Options[key] != savedURL {
						t.Fatal("lost secret reference")
					}
				} else if view.Options[key] != "" || strings.Contains(w.Body.String(), "hidden-") {
					t.Fatal("exposed sensitive URL")
				}
				w = request("POST", "/api/config", edit(map[string]any{"tags": []any{"updated"}}), nil)
				if w.Code != 200 || options[key] != savedURL {
					t.Fatalf("changed saved URL: %d", w.Code)
				}
				w = request("POST", "/api/config", edit(map[string]any{key: "!secret replacement_url"}), nil)
				if w.Code != 200 || options[key] != "!secret replacement_url" {
					t.Fatal("could not save a URL secret reference")
				}
				w = request("POST", "/api/config", edit(map[string]any{key: "https://replacement.example"}), nil)
				if w.Code != 200 || options[key] != "https://replacement.example" {
					t.Fatal("could not explicitly replace URL")
				}
			})
		}
	}
	t.Run("Fleet save lifecycle", func(t *testing.T) {
		options = map[string]any{"tags": []any{}, "collect_host": true}
		w := request("POST", "/api/config", edit(map[string]any{"fleet_url": "https://fleet.example:8220", "enrollment_token": "first-token"}), nil)
		if w.Code != 200 || options["fleet_url"] != "https://fleet.example:8220" || options["enrollment_token"] != "first-token" {
			t.Fatal("Fleet initial save failed")
		}
		w = request("POST", "/api/config", edit(map[string]any{"tags": []any{"fleet"}}), nil)
		if w.Code != 200 || options["enrollment_token"] != "first-token" {
			t.Fatal("Fleet save lost token")
		}
		w = request("POST", "/api/config", edit(map[string]any{"enrollment_token": "replacement-token"}), nil)
		if w.Code != 200 || options["enrollment_token"] != "replacement-token" {
			t.Fatal("Fleet token replacement failed")
		}
		before := saves
		w = request("POST", "/api/config", edit(map[string]any{"enrollment_token": ""}), nil)
		if w.Code != 400 || saves != before || options["enrollment_token"] != "replacement-token" {
			t.Fatal("invalid Fleet save changed stored settings")
		}
		w = request("POST", "/api/config", edit(map[string]any{"fleet_url": "", "enrollment_token": "", "elasticsearch_url": "https://es.example", "api_key": "new-key"}), nil)
		if w.Code != 200 {
			t.Fatal("switch from Fleet failed")
		}
		if _, present := options["fleet_url"]; present || options["enrollment_token"] != "" {
			t.Fatal("Fleet settings were not cleared")
		}
	})

}
func TestValidateSetup(t *testing.T) {
	for _, o := range []map[string]any{
		{}, {"elasticsearch_url": "https://es", "fleet_url": "https://fleet", "enrollment_token": "token"},
		{"fleet_url": "https://fleet"}, {"elasticsearch_url": "file:///etc/passwd"},
		{"elasticsearch_url": "https://user:secret@es"}, {"elasticsearch_url": "https://es/?token=secret"},
		{"elasticsearch_url": "https://es", "password": "only"},
		{"elasticsearch_url": "https://es", "collect_core": false, "collect_host": false, "collect_apps": false, "collect_supervisor": false},
	} {
		if validateSetup(o) == nil {
			t.Errorf("accepted invalid options %v", o)
		}
	}
	for _, o := range []map[string]any{{"elasticsearch_url": "https://es"}, {"fleet_url": "https://fleet", "enrollment_token": "token"}} {
		if err := validateSetup(o); err != nil {
			t.Error(err)
		}
	}
}
func TestSetupReportDoesNotProbeAgent(t *testing.T) {
	dir := t.TempDir()
	opts := filepath.Join(dir, "options.json")
	_ = os.WriteFile(opts, []byte(`{}`), 0600)
	c := NewCollector(Paths{Options: opts, RunDir: dir})
	c.agent = func(context.Context) *AgentStatus { t.Fatal("probed agent in setup mode"); return nil }
	r := c.Report(context.Background(), true)
	if r.Summary.Mode != "setup" || r.Checks[0].Title != "Setup required" {
		t.Fatalf("unexpected setup report %+v", r)
	}
	_ = os.WriteFile(filepath.Join(dir, "startup.json"), []byte(`{"mode":"fleet"}`), 0600)
	_ = os.WriteFile(filepath.Join(dir, "agent-stopped"), nil, 0600)
	r = c.Report(context.Background(), true)
	if r.Summary.Mode != "setup" {
		t.Fatal("stopped agent lost recovery UI")
	}
}

func TestStartupReport(t *testing.T) {
	for _, tc := range []struct {
		name, options    string
		started, stopped bool
		mode, title      string
		status           Status
	}{
		{"unconfigured", `{}`, false, true, "setup", "Setup required", StatusWarn},
		{"starting", `{"elasticsearch_url":"https://es"}`, false, false, "", "Starting", StatusUnknown},
		{"setup failed", `{"elasticsearch_url":"https://es"}`, false, true, "setup", "Startup failed", StatusError},
		{"enrollment refused", `{"fleet_url":"https://fleet","enrollment_token":"t"}`, true, true, "setup", "Startup failed", StatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := filepath.Join(dir, "options.json")
			_ = os.WriteFile(opts, []byte(tc.options), 0600)
			if tc.started {
				_ = os.WriteFile(filepath.Join(dir, "startup.json"), []byte(`{"mode":"fleet"}`), 0600)
			}
			if tc.stopped {
				_ = os.WriteFile(filepath.Join(dir, "agent-stopped"), nil, 0600)
			}
			c := NewCollector(Paths{Options: opts, RunDir: dir})
			c.agent = func(context.Context) *AgentStatus { t.Fatal("probed an agent that isn't running"); return nil }
			r := c.Report(context.Background(), true)
			if r.Summary.Mode != tc.mode || r.Checks[0].Title != tc.title || r.Overall != tc.status {
				t.Fatalf("got mode %q, %q, %s", r.Summary.Mode, r.Checks[0].Title, r.Overall)
			}
		})
	}
}

func TestSupervisorRedirectDoesNotForwardToken(t *testing.T) {
	reached := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer destination.Close()
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer supervisor.Close()
	a := newConfigAPI(Paths{SupervisorURL: supervisor.URL, SupervisorToken: "private"})
	if a.supervisor(context.Background(), "GET", "/addons/self/info", nil, nil) == nil || reached {
		t.Fatal("followed Supervisor redirect")
	}
}
