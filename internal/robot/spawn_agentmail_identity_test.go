package robot

// --robot-spawn provisions each agent pane's Agent Mail identity before the
// agent command is sent, through the coordinator `ntm spawn` uses
// (internal/spawnidentity). Before this, robot- and REST-spawned panes had no
// canonical identity, so reservation-bearing assignment through the robot API
// failed with "pane has no canonical Agent Mail identity", the pane->agent
// registry stayed empty, and pane badges never appeared.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	"github.com/Dicklesworthstone/ntm/internal/assignment"
	"github.com/Dicklesworthstone/ntm/internal/config"
	"github.com/Dicklesworthstone/ntm/internal/tmux"
)

// spawnIdentityMailServer is an MCP JSON-RPC stub (root endpoint, same
// contract as mcp-agent-mail) answering ensure_project and minting one fresh
// name per create_agent_identity call, in order.
type spawnIdentityMailServer struct {
	names []string

	mu       sync.Mutex
	tools    []string
	programs []string
}

func (s *spawnIdentityMailServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     interface{} `json:"id"`
			Method string      `json:"method"`
			Params struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fail := func(msg string) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]interface{}{"code": -32601, "message": msg},
			})
		}
		if req.Method != "tools/call" {
			fail("unknown method")
			return
		}
		s.mu.Lock()
		s.tools = append(s.tools, req.Params.Name)
		var result interface{}
		switch req.Params.Name {
		case "ensure_project":
			result = map[string]interface{}{"id": 7, "slug": "proj", "human_key": req.Params.Arguments["project_key"]}
		case "create_agent_identity":
			program, _ := req.Params.Arguments["program"].(string)
			s.programs = append(s.programs, program)
			created := 0
			for _, tool := range s.tools {
				if tool == "create_agent_identity" {
					created++
				}
			}
			if created > len(s.names) {
				s.mu.Unlock()
				fail("no names left")
				return
			}
			result = map[string]interface{}{
				"id": 40 + created, "name": s.names[created-1], "program": program,
				"model": req.Params.Arguments["model"], "project_id": 7,
			}
		case "register_agent":
			// The session-level coordinator identity (program ntm).
			result = map[string]interface{}{
				"id": 90, "name": "RedStone", "program": req.Params.Arguments["program"],
				"model": req.Params.Arguments["model"], "project_id": 7,
			}
		default:
			s.mu.Unlock()
			fail("unknown tool: " + req.Params.Name)
			return
		}
		s.mu.Unlock()
		raw, _ := json.Marshal(result)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0", "id": req.ID, "result": json.RawMessage(raw),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *spawnIdentityMailServer) calls() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tools...), append([]string(nil), s.programs...)
}

// isolateSpawnIdentityStorage keeps identity files, the session agent
// registry and the badge store out of the developer's real directories.
func isolateSpawnIdentityStorage(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".data"))
}

func agentMailSpawnConfig(enabled bool) *config.Config {
	cfg := testSpawnConfig()
	cfg.AgentMail.Enabled = enabled
	cfg.AgentMail.AutoRegister = true
	return cfg
}

func canonicalIdentity(projectKey, paneID string) string {
	raw, err := os.ReadFile(agentmail.CanonicalIdentityPath(projectKey, paneID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// spawnIdentityPanes is a user pane plus two agent panes with pids, so the
// coordinator's liveness snapshot (read through the GetPanes port) records
// each binding's pane generation.
func spawnIdentityPanes() []tmux.Pane {
	return []tmux.Pane{
		{ID: "%0", WindowIndex: 0, Index: 0, PID: 100},
		{ID: "%1", WindowIndex: 0, Index: 1, PID: 101},
		{ID: "%2", WindowIndex: 0, Index: 2, PID: 102},
	}
}

// TestGetSpawnPublishesAgentMailIdentityBeforeEachLaunch is the robot-spawn
// form of gh#255: when the launch port runs for a pane, that pane's canonical
// identity file and registry binding already name its assigned identity, and
// the envelope reports the same mapping in agent_mail.
func TestGetSpawnPublishesAgentMailIdentityBeforeEachLaunch(t *testing.T) {
	isolateSpawnIdentityStorage(t)
	mail := &spawnIdentityMailServer{names: []string{"BlueLake", "GreenCastle"}}
	srv := mail.start(t)
	t.Setenv("AGENT_MAIL_URL", srv.URL+"/")

	dir := t.TempDir()
	const session = "robot-identity"
	deps := testSpawnLifecycleDependencies(spawnIdentityPanes())
	seenAtLaunch := map[string]string{}
	registryAtLaunch := map[string]string{}
	deps.LaunchAgent = func(_ context.Context, pane tmux.Pane, gotSession, agentType string, number int, _, _ string) (SpawnedAgent, error) {
		seenAtLaunch[pane.ID] = canonicalIdentity(dir, pane.ID)
		if registry, err := agentmail.LoadSessionAgentRegistry(gotSession, dir); err == nil && registry != nil {
			registryAtLaunch[pane.ID], _ = registry.GetAgentByID(pane.ID)
		}
		return SpawnedAgent{
			Pane:  fmt.Sprintf("%d.%d", pane.WindowIndex, pane.Index),
			Type:  agentType,
			Title: fmt.Sprintf("%s__%s_%d", gotSession, agentTypeShort(agentType), number),
		}, nil
	}

	out, err := GetSpawn(t.Context(), SpawnOptions{
		Session: session, CCCount: 1, CodCount: 1, WorkingDir: dir, LifecycleDeps: deps,
	}, agentMailSpawnConfig(true))
	if err != nil || out == nil || !out.Success {
		t.Fatalf("GetSpawn output=%+v err=%v", out, err)
	}

	want := map[string]string{"%1": "BlueLake", "%2": "GreenCastle"}
	for paneID, name := range want {
		if seenAtLaunch[paneID] != name {
			t.Errorf("identity file for %s at launch = %q, want %q (identity must be published before the agent command is sent)", paneID, seenAtLaunch[paneID], name)
		}
		if registryAtLaunch[paneID] != name {
			t.Errorf("registry binding for %s at launch = %q, want %q", paneID, registryAtLaunch[paneID], name)
		}
	}
	if _, userPane := seenAtLaunch["%0"]; userPane {
		t.Error("the user pane must not be launched or given an identity")
	}

	status := out.AgentMail
	if status == nil || !status.Available || !status.ProjectRegistered || status.AgentsRegistered != 2 || status.AgentsFailed != 0 {
		t.Fatalf("agent_mail = %+v, want available, project registered, 2 registered", status)
	}
	for paneID, name := range want {
		if status.AgentMap[paneID] != name {
			t.Errorf("agent_map[%s] = %q, want %q", paneID, status.AgentMap[paneID], name)
		}
	}
	// Each agents[] entry carries its stable pane id and identity, so a
	// caller can join agents[] with agent_map without re-listing panes.
	joined := map[string]string{}
	for _, agent := range out.Agents {
		if agent.Type == "user" {
			if agent.PaneID != "%0" || agent.AgentMailName != "" {
				t.Errorf("user agents[] entry = %+v, want pane_id %%0 and no identity", agent)
			}
			continue
		}
		joined[agent.PaneID] = agent.AgentMailName
	}
	if len(joined) != len(want) || joined["%1"] != want["%1"] || joined["%2"] != want["%2"] {
		t.Errorf("agents[] pane_id -> agent_mail_name = %v, want %v", joined, want)
	}
	// The session-level identity `ntm lock` acts as is registered too, so a
	// robot-spawned session is lockable like an `ntm spawn` one.
	if status.SessionAgent != "RedStone" {
		t.Errorf("agent_mail.session_agent = %q, want RedStone", status.SessionAgent)
	}
	if saved, err := agentmail.LoadSessionAgent(session, dir); err != nil || saved == nil || saved.AgentName != "RedStone" {
		t.Errorf("session agent.json = %+v (err %v), want RedStone persisted for ntm lock", saved, err)
	}

	tools, programs := mail.calls()
	if strings.Join(programs, ",") != "claude-code,codex-cli" {
		t.Errorf("registered programs = %v, want claude-code then codex-cli", programs)
	}
	for _, tool := range tools {
		if tool == "health_check" {
			t.Error("spawn identity registration must not be gated on health_check; ensure_project is the availability probe")
		}
	}

	// The registry is durable and records each pane generation, so a later
	// respawn can tell this binding from a recycled pane id (ntm#256).
	registry, err := agentmail.LoadSessionAgentRegistry(session, dir)
	if err != nil || registry == nil {
		t.Fatalf("load session agent registry: %v (registry=%v)", err, registry)
	}
	if registry.PanePID("%1") != 101 || registry.PanePID("%2") != 102 {
		t.Errorf("recorded pane pids = %d/%d, want 101/102", registry.PanePID("%1"), registry.PanePID("%2"))
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"agent_mail":{"available":true,"project_registered":true,"agents_registered":2`) {
		t.Errorf("robot spawn JSON lacks the agent_mail status: %s", raw)
	}
}

// TestGetSpawnIdentityResolvesReservationRecipient closes the loop on the
// orchestrator-facing failure: reservation-bearing assignment through the
// robot API resolves a robot-spawned pane's recipient from the registry and
// canonical identity file instead of failing with "pane has no canonical
// Agent Mail identity".
func TestGetSpawnIdentityResolvesReservationRecipient(t *testing.T) {
	isolateSpawnIdentityStorage(t)
	mail := &spawnIdentityMailServer{names: []string{"BlueLake"}}
	srv := mail.start(t)
	t.Setenv("AGENT_MAIL_URL", srv.URL+"/")

	dir := t.TempDir()
	const session = "robot-identity-reserve"
	out, err := GetSpawn(t.Context(), SpawnOptions{
		Session: session, CCCount: 1, WorkingDir: dir,
		LifecycleDeps: testSpawnLifecycleDependencies(spawnIdentityPanes()[:2]),
	}, agentMailSpawnConfig(true))
	if err != nil || out == nil || !out.Success {
		t.Fatalf("GetSpawn output=%+v err=%v", out, err)
	}

	reservations, err := newRobotAgentMailReservationRuntime(t.Context(), dir, session, &spawnIdentityReservationClient{projectKey: dir})
	if err != nil {
		t.Fatalf("reservation runtime: %v", err)
	}
	name, err := reservations.ResolveRecipient(t.Context(), dir, session, "%1", "")
	if err != nil || name != "BlueLake" {
		t.Fatalf("ResolveRecipient(%%1) = %q, %v; want BlueLake", name, err)
	}
}

// spawnIdentityReservationClient answers the reservation runtime's project
// and roster reads for the spawned project.
type spawnIdentityReservationClient struct{ projectKey string }

func (c *spawnIdentityReservationClient) EnsureProject(context.Context, string) (*agentmail.Project, error) {
	return &agentmail.Project{ID: 7, HumanKey: c.projectKey}, nil
}

func (c *spawnIdentityReservationClient) ListAgents(context.Context, string) ([]agentmail.Agent, error) {
	return []agentmail.Agent{{Name: "BlueLake", ProjectID: 7}}, nil
}

func (c *spawnIdentityReservationClient) ListReservations(context.Context, string, string, bool) ([]agentmail.FileReservation, error) {
	return nil, nil
}

func (c *spawnIdentityReservationClient) ReservePaths(context.Context, agentmail.FileReservationOptions) (*agentmail.ReservationResult, error) {
	return nil, assignment.GuaranteeNoReservation(fmt.Errorf("not used"))
}

// TestGetSpawnAgentMailUnavailableDoesNotFailSpawn: an Agent Mail server that
// refuses ensure_project never blocks a launch; every agent still launches and
// the envelope reports the degradation.
func TestGetSpawnAgentMailUnavailableDoesNotFailSpawn(t *testing.T) {
	isolateSpawnIdentityStorage(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"down"}}`))
	}))
	t.Cleanup(down.Close)
	t.Setenv("AGENT_MAIL_URL", down.URL+"/")

	dir := t.TempDir()
	deps := testSpawnLifecycleDependencies(spawnIdentityPanes())
	launched := 0
	launch := deps.LaunchAgent
	deps.LaunchAgent = func(ctx context.Context, pane tmux.Pane, session, agentType string, number int, workDir, command string) (SpawnedAgent, error) {
		launched++
		return launch(ctx, pane, session, agentType, number, workDir, command)
	}

	start := time.Now()
	out, err := GetSpawn(t.Context(), SpawnOptions{
		Session: "robot-identity-down", CCCount: 1, CodCount: 1, WorkingDir: dir, LifecycleDeps: deps,
	}, agentMailSpawnConfig(true))
	if err != nil || out == nil || !out.Success {
		t.Fatalf("an unavailable Agent Mail must not fail the spawn: output=%+v err=%v", out, err)
	}
	if launched != 2 {
		t.Fatalf("launched %d agents, want 2", launched)
	}
	status := out.AgentMail
	if status == nil || status.Available || status.ProjectRegistered || status.AgentsFailed != 2 || status.AgentsRegistered != 0 {
		t.Fatalf("agent_mail = %+v, want unavailable with both agents failed", status)
	}
	if got := canonicalIdentity(dir, "%1"); got != "" {
		t.Fatalf("identity file written without a registration: %q", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("degraded spawn took %s; an unavailable server must short-circuit after the first probe", elapsed)
	}
}

// TestGetSpawnAgentMailDisabledIsInert: with registration disabled the robot
// spawn never contacts Agent Mail and the envelope omits agent_mail, exactly
// like `ntm spawn --json`.
func TestGetSpawnAgentMailDisabledIsInert(t *testing.T) {
	isolateSpawnIdentityStorage(t)
	mail := &spawnIdentityMailServer{names: []string{"BlueLake"}}
	srv := mail.start(t)
	t.Setenv("AGENT_MAIL_URL", srv.URL+"/")

	for name, cfg := range map[string]*config.Config{
		"agent_mail disabled": agentMailSpawnConfig(false),
		"auto_register off": func() *config.Config {
			c := agentMailSpawnConfig(true)
			c.AgentMail.AutoRegister = false
			return c
		}(),
		"no config": nil,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			out, err := GetSpawn(t.Context(), SpawnOptions{
				Session: "robot-identity-off", CCCount: 1, WorkingDir: dir,
				LifecycleDeps: testSpawnLifecycleDependencies(spawnIdentityPanes()[:2]),
			}, cfg)
			if err != nil || out == nil || !out.Success {
				t.Fatalf("GetSpawn output=%+v err=%v", out, err)
			}
			if out.AgentMail != nil {
				t.Fatalf("agent_mail = %+v, want absent when registration is disabled", out.AgentMail)
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), `"agent_mail"`) {
				t.Fatalf("robot spawn JSON carries agent_mail while disabled: %s", raw)
			}
			if got := canonicalIdentity(dir, "%1"); got != "" {
				t.Fatalf("identity file written while disabled: %q", got)
			}
		})
	}
	if tools, _ := mail.calls(); len(tools) != 0 {
		t.Fatalf("disabled registration contacted Agent Mail: %v", tools)
	}
}
