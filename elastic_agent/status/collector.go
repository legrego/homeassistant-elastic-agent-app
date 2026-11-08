package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Status is the health of one check, or of the app overall.
type Status string

const (
	StatusOK      Status = "ok"
	StatusInfo    Status = "info" // advice; doesn't affect the overall status
	StatusWarn    Status = "warn"
	StatusError   Status = "error"
	StatusUnknown Status = "unknown"
)

func (s Status) rank() int {
	switch s {
	case StatusError:
		return 3
	case StatusWarn:
		return 2
	case StatusUnknown:
		return 1
	}
	return 0
}

// Check groups, in the order the page shows them.
const (
	GroupAgent        = "Elastic Agent"
	GroupConnectivity = "Connectivity"
	GroupConfig       = "Configuration"
	GroupHost         = "Host"
)

type Check struct {
	ID      string `json:"id"`
	Group   string `json:"group"`
	Title   string `json:"title"`
	Status  Status `json:"status"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
}

type Summary struct {
	Mode            string    `json:"mode"`
	Destination     string    `json:"destination,omitempty"`
	HostName        string    `json:"host_name,omitempty"`
	AgentVersion    string    `json:"agent_version,omitempty"`
	AppVersion      string    `json:"app_version,omitempty"`
	LatestVersion   string    `json:"latest_version,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	StartedAt       time.Time `json:"started_at,omitzero"`
	Tags            []string  `json:"tags"`
}

type LogLine struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

type Report struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Overall     Status       `json:"overall"`
	Summary     Summary      `json:"summary"`
	Checks      []Check      `json:"checks"`
	Agent       *AgentStatus `json:"agent"`
	StartupLog  []LogLine    `json:"startup_log"`
}

// Startup is what the entrypoint found and decided, from startup.json.
type Startup struct {
	Mode             string    `json:"mode"`
	StartedAt        time.Time `json:"started_at"`
	HostName         string    `json:"host_name"`
	HostNameSource   string    `json:"host_name_source"`
	MachineID        string    `json:"machine_id"`
	CAPath           string    `json:"ca_path"`
	Insecure         bool      `json:"insecure"`
	ElasticsearchURL string    `json:"elasticsearch_url"`
	FleetURL         string    `json:"fleet_url"`
	Pipeline         string    `json:"pipeline"`
	Credential       string    `json:"credential"`
	Inputs           []string  `json:"inputs"`
	Tags             []string  `json:"tags"`
	OptionsSHA256    string    `json:"options_sha256"`
}

type Paths struct {
	Options         string
	RunDir          string // startup.json, startup.log and the standalone agent config
	DataDir         string
	JournalDirs     []string
	AgentBinary     string
	SupervisorURL   string
	SupervisorToken string
}

func PathsFromEnv() Paths {
	return Paths{
		Options:         envOr("OPTIONS_FILE", "/data/options.json"),
		RunDir:          envOr("CONFIG_DIR", "/run/ha-elastic-agent"),
		DataDir:         envOr("DATA_DIR", "/data"),
		JournalDirs:     []string{"/var/log/journal", "/run/log/journal"},
		AgentBinary:     envOr("ELASTIC_AGENT_BIN", "elastic-agent"),
		SupervisorURL:   envOr("SUPERVISOR_URL", "http://supervisor"),
		SupervisorToken: os.Getenv("SUPERVISOR_TOKEN"),
	}
}

// Collector builds reports. Remote checks are cached so that several open
// pages don't put load on Elasticsearch.
type Collector struct {
	paths Paths
	now   func() time.Time
	agent func(ctx context.Context) *AgentStatus

	mu         sync.Mutex
	remote     []Check
	remoteAt   time.Time
	appInfo    *appInfo
	appInfoAt  time.Time
	reportedAt time.Time
	report     *Report
}

const (
	remoteTTL      = 30 * time.Second
	minRefresh     = 5 * time.Second
	appInfoTTL     = 10 * time.Minute
	reportTTL      = 3 * time.Second
	startupGrace   = 90 * time.Second
	maxStartupLogs = 300
)

func NewCollector(p Paths) *Collector {
	c := &Collector{paths: p, now: time.Now}
	c.agent = func(ctx context.Context) *AgentStatus { return queryAgent(ctx, p.AgentBinary) }
	return c
}

func (c *Collector) Report(ctx context.Context, refresh bool) *Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.report != nil && now.Sub(c.reportedAt) < reportTTL && !refresh {
		return c.report
	}

	startup, startupErr := readStartup(filepath.Join(c.paths.RunDir, "startup.json"))
	options, optionsRaw, optionsErr := readOptions(c.paths.Options)

	r := &Report{GeneratedAt: now.UTC(), Checks: []Check{}, Summary: Summary{Tags: []string{}}}
	r.StartupLog = readStartupLog(filepath.Join(c.paths.RunDir, "startup.log"))

	if startupErr != nil {
		r.Checks = append(r.Checks, Check{ID: "startup", Group: GroupConfig, Title: "Startup",
			Status: StatusError, Summary: "The app hasn't finished starting, or couldn't record how it started.",
			Detail: startupErr.Error()})
		r.Overall = overall(r.Checks)
		redactReport(r)
		c.report, c.reportedAt = r, now
		return r
	}

	r.Summary = summarize(startup)
	if info := c.cachedAppInfo(ctx, now); info != nil {
		r.Summary.AppVersion = info.Version
		r.Summary.LatestVersion = info.VersionLatest
		r.Summary.UpdateAvailable = info.UpdateAvailable
	}

	r.Agent = c.agent(ctx)
	if r.Agent != nil && r.Agent.Version != "" {
		r.Summary.AgentVersion = r.Agent.Version
	}

	r.Checks = append(r.Checks, agentChecks(startup, r.Agent, now)...)
	r.Checks = append(r.Checks, c.cachedRemote(ctx, startup, now, refresh)...)
	r.Checks = append(r.Checks, configChecks(startup, options, optionsRaw, optionsErr, c.paths)...)
	r.Checks = append(r.Checks, hostChecks(startup, c.paths)...)
	r.Overall = overall(r.Checks)
	redactReport(r)

	c.report, c.reportedAt = r, now
	return r
}

func (c *Collector) cachedRemote(ctx context.Context, s *Startup, now time.Time, refresh bool) []Check {
	age := now.Sub(c.remoteAt)
	if c.remote != nil && (age < remoteTTL && !(refresh && age >= minRefresh)) {
		return c.remote
	}
	switch s.Mode {
	case "standalone":
		c.remote = elasticsearchChecks(ctx, c.paths, s)
	case "fleet":
		c.remote = fleetServerChecks(ctx, s)
	default:
		c.remote = []Check{}
	}
	c.remoteAt = now
	return c.remote
}

func (c *Collector) cachedAppInfo(ctx context.Context, now time.Time) *appInfo {
	if c.appInfo != nil && now.Sub(c.appInfoAt) < appInfoTTL {
		return c.appInfo
	}
	if info := querySupervisor(ctx, c.paths.SupervisorURL, c.paths.SupervisorToken); info != nil {
		c.appInfo, c.appInfoAt = info, now
	}
	return c.appInfo
}

func overall(checks []Check) Status {
	worst := StatusOK
	for _, ch := range checks {
		if ch.Status.rank() > worst.rank() {
			worst = ch.Status
		}
	}
	return worst
}

func summarize(s *Startup) Summary {
	sum := Summary{Mode: s.Mode, HostName: s.HostName, StartedAt: s.StartedAt, Tags: s.Tags}
	if sum.Tags == nil {
		sum.Tags = []string{}
	}
	switch s.Mode {
	case "standalone":
		sum.Destination = redactURL(s.ElasticsearchURL)
	case "fleet":
		sum.Destination = redactURL(s.FleetURL)
	}
	return sum
}

// --- Configuration ------------------------------------------------------------

var sourceNames = []struct{ option, label string }{
	{"collect_core", "Home Assistant Core"},
	{"collect_supervisor", "Supervisor"},
	{"collect_apps", "Apps"},
	{"collect_host", "Host"},
}

func configChecks(s *Startup, opts map[string]any, raw []byte, optsErr error, p Paths) []Check {
	var checks []Check

	switch s.Mode {
	case "standalone":
		checks = append(checks, Check{ID: "mode", Group: GroupConfig, Title: "Mode", Status: StatusOK,
			Summary: "Standalone. The agent sends logs straight to " + redactURL(s.ElasticsearchURL) + "."})
	case "fleet":
		checks = append(checks, Check{ID: "mode", Group: GroupConfig, Title: "Mode", Status: StatusOK,
			Summary: "Fleet. The policy assigned in Kibana decides what the agent collects and where it sends it.",
			Detail:  "Fleet Server: " + redactURL(s.FleetURL)})
	default:
		checks = append(checks, Check{ID: "mode", Group: GroupConfig, Title: "Mode", Status: StatusError,
			Summary: fmt.Sprintf("Unknown mode %q.", s.Mode)})
	}

	if s.Mode == "standalone" {
		checks = append(checks, credentialCheck(s, p))
		if optsErr == nil && optionString(opts, "password") != "" && (s.Credential == "saved" || s.Credential == "created") {
			checks = append(checks, Check{ID: "password", Group: GroupConfig, Title: "Setup password", Status: StatusInfo,
				Summary: "You can remove the username and password from the configuration. The app sends logs with its own API key."})
		}
		checks = append(checks, sourcesCheck(s, opts, optsErr))
	}

	tls := Check{ID: "tls", Group: GroupConfig, Title: "Certificate verification"}
	switch {
	case s.Insecure:
		tls.Status = StatusWarn
		tls.Summary = "Off. Server certificates aren't verified, so use this only on test clusters."
	case s.CAPath != "":
		tls.Status = StatusOK
		tls.Summary = "On, with the configured CA certificate."
		tls.Detail = s.CAPath
	default:
		tls.Status = StatusOK
		tls.Summary = "On, with the system's trusted CAs."
	}
	checks = append(checks, tls)

	saved := Check{ID: "options_changed", Group: GroupConfig, Title: "Saved configuration"}
	switch {
	case optsErr != nil:
		saved.Status = StatusWarn
		saved.Summary = "Can't read the app options."
		saved.Detail = optsErr.Error()
	case s.OptionsSHA256 != "" && sha256Hex(raw) != s.OptionsSHA256:
		saved.Status = StatusWarn
		saved.Summary = "The configuration changed after the app started. Restart the app to apply it."
	default:
		saved.Status = StatusOK
		saved.Summary = "The app is running with the saved configuration."
	}
	checks = append(checks, saved)
	return checks
}

func credentialCheck(s *Startup, p Paths) Check {
	ch := Check{ID: "credential", Group: GroupConfig, Title: "API key", Status: StatusOK}
	switch s.Credential {
	case "configured":
		ch.Summary = "Sending logs with the API key from the configuration."
	case "created", "saved":
		ch.Summary = "Sending logs with the write-only API key the app created."
		if s.Credential == "created" {
			ch.Summary = "Created a write-only API key on this start, and sending logs with it."
		}
		if id, name := savedKeyName(filepath.Join(p.DataDir, "elasticsearch-api-key.json")); id != "" {
			ch.Detail = fmt.Sprintf("%s (ID %s), under Stack Management > API keys", name, id)
		}
	default:
		ch.Status = StatusUnknown
		ch.Summary = "Unknown."
	}
	return ch
}

func sourcesCheck(s *Startup, opts map[string]any, optsErr error) Check {
	ch := Check{ID: "sources", Group: GroupConfig, Title: "Log sources", Status: StatusOK}
	if optsErr != nil {
		ch.Status = StatusUnknown
		ch.Summary = "Can't read the app options."
		return ch
	}
	var on, off []string
	for _, src := range sourceNames {
		if optionBool(opts, src.option, true) {
			on = append(on, src.label)
		} else {
			off = append(off, src.label)
		}
	}
	ch.Summary = "Sending " + joinList(on) + " logs."
	if len(off) > 0 {
		ch.Detail = "Turned off: " + joinList(off) + "."
	}
	if len(on) == 0 {
		ch.Status = StatusError
		ch.Summary = "Every log source is turned off."
	}
	return ch
}

// --- Host ---------------------------------------------------------------------

func hostChecks(s *Startup, p Paths) []Check {
	host := Check{ID: "host_name", Group: GroupHost, Title: "Host name"}
	if s.HostNameSource == "supervisor" {
		host.Status = StatusOK
		host.Summary = "Logs are labelled with " + s.HostName + "."
	} else {
		host.Status = StatusWarn
		host.Summary = "Couldn't read the host name from the Supervisor. Logs are labelled with " + s.HostName + " instead."
	}

	journal := Check{ID: "journal", Group: GroupHost, Title: "Host journal"}
	found := journalFiles(p.JournalDirs, s.MachineID)
	switch {
	case s.MachineID == "":
		journal.Status = StatusError
		journal.Summary = "No host journal was found when the app started."
		journal.Detail = "Looked in " + strings.Join(p.JournalDirs, " and ") + "."
	case found == 0:
		journal.Status = StatusError
		journal.Summary = "The host journal found at startup is gone."
		journal.Detail = "Machine ID " + s.MachineID
	default:
		journal.Status = StatusOK
		journal.Summary = fmt.Sprintf("Reading the host journal (%d %s).", found, plural(found, "file", "files"))
		journal.Detail = "Machine ID " + s.MachineID
	}
	return []Check{host, journal}
}

func journalFiles(dirs []string, machineID string) int {
	if machineID == "" {
		return 0
	}
	n := 0
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, machineID, "*.journal*"))
		n += len(matches)
	}
	return n
}

// --- Files --------------------------------------------------------------------

func readStartup(path string) (*Startup, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Startup
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func readOptions(path string) (map[string]any, []byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, b, fmt.Errorf("%s: %w", path, err)
	}
	return m, b, nil
}

func readStartupLog(path string) []LogLine {
	lines := []LogLine{}
	f, err := os.Open(path)
	if err != nil {
		return lines
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		text := sc.Text()
		level := "info"
		switch {
		case strings.Contains(text, "ERROR:"):
			level = "error"
		case strings.Contains(text, "WARNING:"):
			level = "warn"
		}
		lines = append(lines, LogLine{Level: level, Text: text})
	}
	if len(lines) > maxStartupLogs {
		lines = lines[len(lines)-maxStartupLogs:]
	}
	return lines
}

func savedKeyName(path string) (id, name string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var k struct{ ID, Name string }
	if json.Unmarshal(b, &k) != nil {
		return "", ""
	}
	return k.ID, k.Name
}

// --- Helpers ------------------------------------------------------------------

func optionString(opts map[string]any, key string) string {
	s, _ := opts[key].(string)
	return s
}

func optionBool(opts map[string]any, key string, fallback bool) bool {
	if b, ok := opts[key].(bool); ok {
		return b
	}
	return fallback
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// redactURL drops any user name or password from a URL before it's shown.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.User = nil
	return u.String()
}

// urlUserinfo matches the user:password@ part of any URL in free text.
var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/?#\s@]+@`)

// redactText removes credentials from URLs anywhere in s. Startup messages
// and error text can quote the URL the user configured, password and all.
func redactText(s string) string {
	return urlUserinfo.ReplaceAllString(s, "$1")
}

// redactReport strips URL credentials from every free-text field in r, so no
// message can carry them to the page.
func redactReport(r *Report) {
	for i := range r.StartupLog {
		r.StartupLog[i].Text = redactText(r.StartupLog[i].Text)
	}
	for i := range r.Checks {
		r.Checks[i].Summary = redactText(r.Checks[i].Summary)
		r.Checks[i].Detail = redactText(r.Checks[i].Detail)
	}
	if a := r.Agent; a != nil {
		a.Error, a.Message, a.FleetMessage = redactText(a.Error), redactText(a.Message), redactText(a.FleetMessage)
		for ci := range a.Components {
			c := &a.Components[ci]
			c.Message = redactText(c.Message)
			for ui := range c.Units {
				u := &c.Units[ui]
				u.Message = redactText(u.Message)
				for si := range u.Streams {
					u.Streams[si].Message = redactText(u.Streams[si].Message)
				}
			}
		}
	}
}

func joinList(items []string) string {
	switch len(items) {
	case 0:
		return "no"
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
