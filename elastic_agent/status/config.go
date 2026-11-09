package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Only these options can cross the browser boundary. Secrets are write-only.
var configFields = map[string]string{
	"elasticsearch_url": "string", "username": "string", "password": "secret", "api_key": "secret",
	"fleet_url": "string", "enrollment_token": "secret", "ca_certificate": "string",
	"collect_core": "bool", "collect_supervisor": "bool", "collect_apps": "bool", "collect_host": "bool",
	"read_existing_logs": "bool", "insecure": "bool", "tags": "tags",
}

type configAPI struct {
	paths Paths
	key   []byte
	mu    sync.Mutex
}

func newConfigAPI(p Paths) *configAPI {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return &configAPI{paths: p, key: key}
}
func (a *configAPI) sign(s string) string {
	m := hmac.New(sha256.New, a.key)
	_, _ = m.Write([]byte(s))
	return hex.EncodeToString(m.Sum(nil))
}
func (a *configAPI) revision(opts map[string]any) string {
	b, _ := json.Marshal(opts)
	return a.sign("revision:" + string(b))
}
func (a *configAPI) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/config", a.guard(a.get))
	mux.HandleFunc("POST /api/config", a.guard(a.save))
	mux.HandleFunc("POST /api/restart", a.guard(a.restart))
}
func (a *configAPI) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// Never trust identity headers from loopback, LAN clients or development
		// allow-list overrides. Only Supervisor sanitizes and supplies these headers.
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		admin := false
		for _, g := range strings.Split(r.Header.Get("X-Remote-User-Groups"), ",") {
			if strings.TrimSpace(g) == "system-admin" {
				admin = true
			}
		}
		user := r.Header.Get("X-Remote-User-Id")
		if host != ingressProxyIP || user == "" || !admin {
			http.Error(w, "Configuration requires a verified Home Assistant administrator. If your Supervisor does not supply user groups, update it or use the app's Configuration tab.", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
			http.Error(w, "Cross-origin request refused.", 403)
			return
		}
		if r.Method == http.MethodPost {
			if !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(a.sign("csrf:"+user))) {
				http.Error(w, "Reload the page before saving.", 403)
				return
			}
			typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || typ != "application/json" {
				http.Error(w, "JSON required.", 415)
				return
			}
		}
		next(w, r)
	}
}
func (a *configAPI) supervisor(ctx context.Context, method, path string, body any, out any) error {
	if a.paths.SupervisorToken == "" {
		return errors.New("Supervisor unavailable")
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.paths.SupervisorURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.paths.SupervisorToken)
	req.Header.Set("Content-Type", "application/json")
	// Never forward the Supervisor token on redirects, including same-host ones.
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Supervisor unavailable")
	}
	defer resp.Body.Close()
	var envelope struct {
		Result string          `json:"result"`
		Data   json.RawMessage `json:"data"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope) != nil || envelope.Result != "ok" {
		return errors.New("Supervisor rejected the request")
	}
	if out != nil {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}
func (a *configAPI) options(ctx context.Context) (map[string]any, error) {
	var info struct {
		Options map[string]any `json:"options"`
	}
	err := a.supervisor(ctx, "GET", "/addons/self/info", nil, &info)
	if err == nil && info.Options == nil {
		err = errors.New("Supervisor returned no options")
	}
	return info.Options, err
}
func writeConfigJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (a *configAPI) get(w http.ResponseWriter, r *http.Request) {
	opts, err := a.options(r.Context())
	if err != nil {
		http.Error(w, "Unable to read settings from Supervisor. Try again.", 502)
		return
	}
	visible := map[string]any{}
	secrets := map[string]bool{}
	for k, kind := range configFields {
		if kind == "secret" {
			secrets[k] = optionString(opts, k) != ""
			continue
		}
		if v, ok := opts[k]; ok {
			visible[k] = v
		}
	}
	// Keep sensitive URLs write-only. Never turn a redacted display value into
	// a replacement URL: the client omits unchanged fields on save.
	urls := map[string]bool{}
	for _, k := range []string{"elasticsearch_url", "fleet_url"} {
		v := optionString(opts, k)
		urls[k] = v != ""
		if strings.HasPrefix(v, "!secret ") {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			visible[k] = ""
		}
	}
	mode := "standalone"
	if optionString(opts, "fleet_url") != "" {
		mode = "fleet"
	}
	writeConfigJSON(w, map[string]any{"options": visible, "mode": mode, "configured_urls": urls, "configured_secrets": secrets, "revision": a.revision(opts), "csrf_token": a.sign("csrf:" + r.Header.Get("X-Remote-User-Id"))})
}

type configEdit struct {
	Options  map[string]any `json:"options"`
	Revision string         `json:"revision"`
}

func (a *configAPI) save(w http.ResponseWriter, r *http.Request) {
	var edit configEdit
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	dec.DisallowUnknownFields()
	if dec.Decode(&edit) != nil || dec.Decode(new(any)) != io.EOF || edit.Options == nil {
		http.Error(w, "Invalid settings request.", 400)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	opts, err := a.options(r.Context())
	if err != nil {
		http.Error(w, "Unable to read current settings. Nothing was saved.", 502)
		return
	}
	if !hmac.Equal([]byte(edit.Revision), []byte(a.revision(opts))) {
		http.Error(w, "Settings changed. Reload the page before saving.", 409)
		return
	}
	for k, v := range edit.Options {
		kind, ok := configFields[k]
		if !ok {
			http.Error(w, "Unknown setting.", 400)
			return
		}
		valid := false
		switch kind {
		case "string", "secret":
			_, valid = v.(string)
		case "bool":
			_, valid = v.(bool)
		case "tags":
			if tags, ok := v.([]any); ok {
				valid = true
				for _, tag := range tags {
					if _, ok := tag.(string); !ok {
						valid = false
					}
				}
			}
		}
		if !valid {
			http.Error(w, "Invalid setting type.", 400)
			return
		}
		// Omitted secrets are preserved; an explicit empty string clears them.
		if k == "fleet_url" && v == "" {
			delete(opts, k)
		} else {
			opts[k] = v
		}
	}
	if err := validateSetupEdit(opts, edit.Options); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := a.supervisor(r.Context(), "POST", "/addons/self/options", map[string]any{"options": opts}, nil); err != nil {
		http.Error(w, "Supervisor could not confirm the save. Reload settings before trying again.", 502)
		return
	}
	writeConfigJSON(w, map[string]any{"saved": true})
}
func validateSetup(o map[string]any) error { return validateSetupEdit(o, o) }

func validateSetupEdit(o, changes map[string]any) error {
	es, fleet, token := optionString(o, "elasticsearch_url"), optionString(o, "fleet_url"), optionString(o, "enrollment_token")
	if es != "" && (fleet != "" || token != "") {
		return errors.New("Choose standalone or Fleet, not both.")
	}
	if es == "" && (fleet == "" || token == "") {
		return errors.New("Enter an Elasticsearch URL, or a Fleet URL and enrollment token.")
	}
	for _, k := range []string{"elasticsearch_url", "fleet_url"} {
		v := optionString(o, k)
		if v == "" {
			continue
		}
		// Existing URLs, including secret references and legacy URL credentials,
		// survive unrelated edits verbatim. Supervisor validates secret resolution.
		if _, changed := changes[k]; !changed {
			continue
		}
		if strings.HasPrefix(v, "!secret ") && strings.TrimSpace(strings.TrimPrefix(v, "!secret ")) != "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%s must be an HTTP(S) URL without credentials, query parameters or fragments.", k)
		}
	}
	if es != "" {
		on := false
		for _, s := range sourceNames {
			on = on || optionBool(o, s.option, true)
		}
		if !on {
			return errors.New("Enable at least one log source.")
		}
		// A previously generated API key may be reused without setup credentials.
		if optionString(o, "password") != "" && optionString(o, "username") == "" {
			return errors.New("Supply a username with the password, or clear the password.")
		}
	}
	return nil
}
func (a *configAPI) restart(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	if dec.Decode(&body) != nil || len(body) != 0 || dec.Decode(new(any)) != io.EOF {
		http.Error(w, "Expected an empty JSON object.", 400)
		return
	}
	// A self-restart may close the ingress connection before Supervisor replies.
	if err := a.supervisor(r.Context(), "POST", "/addons/self/restart", nil, nil); err != nil {
		http.Error(w, "Restart could not be confirmed. Check status or restart from Home Assistant.", 502)
		return
	}
	writeConfigJSON(w, map[string]bool{"restarted": true})
}
