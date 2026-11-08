package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// AgentStatus is the useful part of `elastic-agent status --output json`.
type AgentStatus struct {
	Running      bool        `json:"running"`
	Error        string      `json:"error,omitempty"`
	ID           string      `json:"id,omitempty"`
	Version      string      `json:"version,omitempty"`
	Managed      bool        `json:"managed"`
	State        string      `json:"state,omitempty"`
	Message      string      `json:"message,omitempty"`
	FleetState   string      `json:"fleet_state,omitempty"`
	FleetMessage string      `json:"fleet_message,omitempty"`
	Components   []Component `json:"components"`
}

type Component struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Message string `json:"message"`
	Units   []Unit `json:"units"`
}

type Unit struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	State   string   `json:"state"`
	Message string   `json:"message"`
	Streams []Stream `json:"streams,omitempty"`
}

type Stream struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// Elastic Agent's client.State values, which the status JSON gives as numbers.
var agentStates = []string{"starting", "configuring", "healthy", "degraded", "failed", "stopping", "stopped", "upgrading", "rollback"}

func stateName(n int) string {
	if n >= 0 && n < len(agentStates) {
		return agentStates[n]
	}
	return fmt.Sprintf("unknown (%d)", n)
}

func stateStatus(state string) Status {
	switch state {
	case "healthy":
		return StatusOK
	case "failed":
		return StatusError
	}
	return StatusWarn
}

type rawAgentStatus struct {
	Info struct {
		ID        string `json:"id"`
		Version   string `json:"version"`
		IsManaged bool   `json:"is_managed"`
	} `json:"info"`
	State        int    `json:"state"`
	Message      string `json:"message"`
	FleetState   int    `json:"FleetState"`
	FleetMessage string `json:"FleetMessage"`
	Components   []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		State   int    `json:"state"`
		Message string `json:"message"`
		Units   []struct {
			UnitID   string `json:"unit_id"`
			UnitType int    `json:"unit_type"`
			State    int    `json:"state"`
			Message  string `json:"message"`
			Payload  *struct {
				Streams map[string]struct {
					Status string `json:"status"`
					Error  string `json:"error"`
				} `json:"streams"`
			} `json:"payload"`
		} `json:"units"`
	} `json:"components"`
}

// queryAgent asks the running agent for its status over its control socket.
// The agent finds the socket from STATE_PATH, which the entrypoint sets.
func queryAgent(ctx context.Context, binary string) *AgentStatus {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, "status", "--output", "json")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	// The command exits non-zero when the agent is unhealthy, but still prints
	// its status.
	if st, err := parseAgentStatus(stdout.Bytes()); err == nil {
		return st
	}
	msg := firstLine(stderr.String())
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	return &AgentStatus{Running: false, Error: msg, Components: []Component{}}
}

func parseAgentStatus(b []byte) (*AgentStatus, error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return nil, fmt.Errorf("not JSON")
	}
	var raw rawAgentStatus
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	st := &AgentStatus{
		Running:      true,
		ID:           raw.Info.ID,
		Version:      raw.Info.Version,
		Managed:      raw.Info.IsManaged,
		State:        stateName(raw.State),
		Message:      raw.Message,
		FleetState:   stateName(raw.FleetState),
		FleetMessage: raw.FleetMessage,
		Components:   []Component{},
	}
	for _, rc := range raw.Components {
		c := Component{ID: rc.ID, Name: rc.Name, State: stateName(rc.State), Message: rc.Message, Units: []Unit{}}
		for _, ru := range rc.Units {
			u := Unit{ID: ru.UnitID, Type: "input", State: stateName(ru.State), Message: ru.Message}
			if ru.UnitType == 1 {
				u.Type = "output"
			}
			if ru.Payload != nil {
				for id, s := range ru.Payload.Streams {
					u.Streams = append(u.Streams, Stream{ID: id, Status: strings.ToLower(s.Status), Message: s.Error})
				}
				sort.Slice(u.Streams, func(i, j int) bool { return u.Streams[i].ID < u.Streams[j].ID })
			}
			c.Units = append(c.Units, u)
		}
		st.Components = append(st.Components, c)
	}
	return st, nil
}

func agentChecks(s *Startup, a *AgentStatus, now time.Time) []Check {
	starting := !s.StartedAt.IsZero() && now.Sub(s.StartedAt) < startupGrace
	main := Check{ID: "agent", Group: GroupAgent, Title: "Agent"}
	if a == nil || !a.Running {
		if a != nil && s.Mode != "fleet" {
			main.Detail = a.Error
		}
		switch {
		case s.Mode == "fleet":
			// In Fleet mode the agent only starts once it has enrolled.
			main.Status = StatusWarn
			main.Summary = "Not running yet. Elastic Agent starts after it enrolls with Fleet Server. If this lasts, check the Fleet Server check and the Log tab."
		case starting:
			main.Status = StatusWarn
			main.Summary = "Starting."
		default:
			main.Status = StatusError
			main.Summary = "Elastic Agent isn't answering. See the Log tab."
		}
		checks := []Check{main}
		if s.Mode == "fleet" {
			checks = append(checks, Check{ID: "fleet", Group: GroupAgent, Title: "Fleet enrollment",
				Status: StatusWarn, Summary: "Not enrolled yet."})
		}
		return checks
	}

	main.Status = stateStatus(a.State)
	if starting && main.Status == StatusWarn && (a.State == "starting" || a.State == "configuring") {
		main.Status = StatusInfo
	}
	main.Summary = capitalize(a.State) + "."
	if a.Message != "" && !strings.EqualFold(a.Message, a.State) && a.Message != "Running" {
		main.Summary = capitalize(a.State) + ": " + a.Message
	}
	checks := []Check{main}

	if s.Mode == "fleet" {
		fleet := Check{ID: "fleet", Group: GroupAgent, Title: "Fleet connection", Summary: a.FleetMessage}
		switch a.FleetState {
		case "healthy":
			fleet.Status = StatusOK
		case "failed":
			fleet.Status = StatusError
		default:
			fleet.Status = StatusWarn
		}
		if fleet.Summary == "" {
			fleet.Summary = capitalize(a.FleetState) + "."
		}
		checks = append(checks, fleet)
	}

	inputs := Check{ID: "components", Group: GroupAgent, Title: "Inputs and outputs"}
	var problems []string
	units, worst := 0, StatusOK
	for _, c := range a.Components {
		for _, u := range c.Units {
			units++
			st := stateStatus(u.State)
			if st.rank() > worst.rank() {
				worst = st
			}
			if st != StatusOK {
				msg := u.Message
				if msg == "" {
					msg = u.State
				}
				problems = append(problems, fmt.Sprintf("%s: %s", u.ID, msg))
			}
		}
	}
	switch {
	case units == 0 && s.Mode == "fleet":
		inputs.Status = StatusWarn
		inputs.Summary = "The agent policy has no integrations yet. Add one in Kibana under Fleet > Agent policies."
	case units == 0:
		inputs.Status = StatusWarn
		inputs.Summary = "The agent isn't running any inputs yet."
	case len(problems) == 0:
		inputs.Status = StatusOK
		inputs.Summary = fmt.Sprintf("All %d %s are healthy.", units, plural(units, "unit", "units"))
	default:
		inputs.Status = worst
		if starting && worst == StatusWarn {
			inputs.Status = StatusInfo
		}
		inputs.Summary = fmt.Sprintf("%d of %d %s need attention.", len(problems), units, plural(units, "unit", "units"))
		inputs.Detail = strings.Join(problems, "\n")
	}
	return append(checks, inputs)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "Error: ")
}
