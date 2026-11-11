package lifecycle

import (
	"errors"
	"strings"
	"testing"

	"github.com/dac-plane/dac-plane/internal/ids"
)

func newAgentProfile(t *testing.T) AgentProfile {
	t.Helper()
	return AgentProfile{
		Name: "Code reviewer", Role: "Reviews changes for correctness",
		Instructions: "Read the whole change before commenting.",
		Provider:     newID[ids.Provider](t), EnvironmentProvider: newID[ids.Provider](t),
	}
}

func newAgent(t *testing.T) (Agent, AgentRevision) {
	t.Helper()
	agent, revision, err := NewAgent(newID[ids.Agent](t), newID[ids.Tenant](t), newID[ids.Principal](t), newAgentProfile(t), testNow)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	return agent, revision
}

func TestNewAgentStartsActiveAtRevisionOne(t *testing.T) {
	agent, revision := newAgent(t)
	if agent.State != AgentActive || agent.Revision != 1 || agent.Version != 1 {
		t.Fatalf("agent = %+v, want active at revision 1, version 1", agent)
	}
	if revision.Number != 1 || revision.Agent != agent.ID || revision.AgentProfile != agent.AgentProfile || revision.CreatedBy != agent.CreatedBy {
		t.Fatalf("revision = %+v, want the agent's first profile", revision)
	}
	if err := revision.Validate(); err != nil {
		t.Fatalf("revision.Validate() error = %v", err)
	}
	if reference := agent.Reference(); reference.Agent != agent.ID || reference.Revision != 1 || reference.Validate() != nil {
		t.Fatalf("Reference() = %+v", reference)
	}
}

func TestAgentProfileBounds(t *testing.T) {
	cases := map[string]func(*AgentProfile){
		"empty name":            func(p *AgentProfile) { p.Name = "" },
		"padded name":           func(p *AgentProfile) { p.Name = " Reviewer" },
		"two line name":         func(p *AgentProfile) { p.Name = "Code\nreviewer" },
		"long name":             func(p *AgentProfile) { p.Name = strings.Repeat("n", MaxAgentNameCharacters+1) },
		"invalid name encoding": func(p *AgentProfile) { p.Name = "\xff" },
		"blank role":            func(p *AgentProfile) { p.Role = " \n" },
		"long role":             func(p *AgentProfile) { p.Role = strings.Repeat("r", MaxAgentRoleCharacters+1) },
		"blank instructions":    func(p *AgentProfile) { p.Instructions = "\t" },
		"long instructions":     func(p *AgentProfile) { p.Instructions = strings.Repeat("i", MaxAgentInstructionsCharacters+1) },
		"no runtime provider":   func(p *AgentProfile) { p.Provider = ids.ProviderID{} },
		"no environment":        func(p *AgentProfile) { p.EnvironmentProvider = ids.ProviderID{} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			profile := newAgentProfile(t)
			change(&profile)
			_, _, err := NewAgent(newID[ids.Agent](t), newID[ids.Tenant](t), newID[ids.Principal](t), profile, testNow)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("NewAgent() error = %v, want ErrInvalid", err)
			}
		})
	}

	longest := newAgentProfile(t)
	longest.Name = strings.Repeat("é", MaxAgentNameCharacters)
	longest.Instructions = ""
	if _, _, err := NewAgent(newID[ids.Agent](t), newID[ids.Tenant](t), newID[ids.Principal](t), longest, testNow); err != nil {
		t.Fatalf("NewAgent() with the longest name and no instructions: %v", err)
	}
}

func TestAgentReviseKeepsEveryRevision(t *testing.T) {
	agent, first := newAgent(t)
	profile := agent.AgentProfile
	profile.Instructions = "Comment only on defects."

	revised, second, change, err := agent.Revise(agent.CreatedBy, profile, agent.Version, testNow)
	if err != nil {
		t.Fatalf("Revise() error = %v", err)
	}
	if revised.Revision != 2 || revised.Version != 2 || revised.State != AgentActive || revised.AgentProfile != profile {
		t.Fatalf("revised = %+v, want revision 2 with the new profile", revised)
	}
	if second.Number != 2 || second.AgentProfile != profile || first.AgentProfile == second.AgentProfile {
		t.Fatalf("revisions = %+v then %+v, want two distinct profiles", first, second)
	}
	if err := change.Audit(newID[ids.Audit](t), Actor{Kind: ActorPrincipal, Principal: agent.CreatedBy}, testNow).Validate(); err != nil {
		t.Fatalf("revise audit: %v", err)
	}
	if change.Record != RecordAgent || change.Event != string(AgentRevise) || change.Version != 2 {
		t.Fatalf("change = %+v", change)
	}

	invalid := profile
	invalid.Role = ""
	for name, refused := range map[string]AgentProfile{"unchanged": profile, "invalid": invalid} {
		kept, revision, change, err := revised.Revise(agent.CreatedBy, refused, revised.Version, testNow)
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s Revise() error = %v, want ErrInvalid", name, err)
		}
		if kept != revised || revision != (AgentRevision{}) {
			t.Fatalf("%s Revise() changed the agent or returned a revision: %+v, %+v", name, kept, revision)
		}
		if change.To != "" || change.Version != revised.Version || change.From != string(AgentActive) {
			t.Fatalf("%s Revise() change = %+v, want unapplied at version %d", name, change, revised.Version)
		}
	}
	_, _, _, err = revised.Revise(agent.CreatedBy, newAgentProfile(t), agent.Version, testNow)
	testRejectCode(t, err, RejectVersionConflict)
}

func TestOnlyTheCreatorChangesAnAgent(t *testing.T) {
	agent, _ := newAgent(t)
	other := newID[ids.Principal](t)

	_, _, change, err := agent.Revise(other, newAgentProfile(t), agent.Version, testNow)
	testRejectCode(t, err, RejectUnauthorizedActor)
	if change.To != "" || change.Version != agent.Version {
		t.Fatalf("refused change = %+v, want no target and the unchanged version", change)
	}
	_, _, err = agent.Retire(other, agent.Version, testNow)
	testRejectCode(t, err, RejectUnauthorizedActor)
	_, _, err = agent.Retire(ids.PrincipalID{}, agent.Version, testNow)
	testRejectCode(t, err, RejectUnauthorizedActor)
}

func TestRetiredAgentAcceptsNoChange(t *testing.T) {
	agent, _ := newAgent(t)
	retired, change, err := agent.Retire(agent.CreatedBy, agent.Version, testNow)
	if err != nil {
		t.Fatalf("Retire() error = %v", err)
	}
	if retired.State != AgentRetired || retired.Revision != 1 || retired.Version != 2 || change.To != string(AgentRetired) {
		t.Fatalf("retired = %+v, change = %+v", retired, change)
	}
	if err := retired.Validate(); err != nil {
		t.Fatalf("retired.Validate() error = %v", err)
	}
	_, _, _, err = retired.Revise(agent.CreatedBy, newAgentProfile(t), retired.Version, testNow)
	testRejectCode(t, err, RejectTerminalState)
	_, _, err = retired.Retire(agent.CreatedBy, retired.Version, testNow)
	testRejectCode(t, err, RejectTerminalState)
}

func TestTaskAgentReferenceIsValidated(t *testing.T) {
	agent, _ := newAgent(t)
	task := newTask(t)
	reference := agent.Reference()
	task.Agent = &reference
	if err := task.Validate(); err != nil {
		t.Fatalf("delegated task: %v", err)
	}
	task.Agent = &AgentReference{Agent: agent.ID}
	if err := task.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("task with a half reference: error = %v, want ErrInvalid", err)
	}
	task.Agent = &AgentReference{}
	if err := task.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("task with an empty reference: error = %v, want ErrInvalid", err)
	}
}
