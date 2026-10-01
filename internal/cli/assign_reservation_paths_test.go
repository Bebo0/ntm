package cli

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Dicklesworthstone/ntm/internal/agentmail"
	assignpkg "github.com/Dicklesworthstone/ntm/internal/assign"
	"github.com/Dicklesworthstone/ntm/internal/assignment"
	"github.com/Dicklesworthstone/ntm/internal/bv"
)

// GH #336: the bead's output paths are in its description, not its title.
const gh336Description = "Read README.md and docs/research-status.md.\n\n" +
	"## Owned outputs\n\n" +
	"- `docs/specification/work/inventory.md`\n" +
	"- `docs/evidence/inventory.md`\n\n" +
	"## Acceptance criteria\n\n" +
	"- Inventory the modules and cite evidence.\n"

var gh336Outputs = []string{"docs/evidence/inventory.md", "docs/specification/work/inventory.md"}

func TestAssignmentReservationPathsReadsTheDescription(t *testing.T) {
	details := &bv.BeadAssignmentDetails{ID: "ntm-336", Title: "Inventory modules and entry points", Description: gh336Description}

	paths, err := assignmentReservationPaths(details, true)
	if err != nil {
		t.Fatalf("assignmentReservationPaths: %v", err)
	}
	sort.Strings(paths)
	if !reflect.DeepEqual(paths, gh336Outputs) {
		t.Fatalf("paths = %v, want the owned outputs %v", paths, gh336Outputs)
	}

	if paths, err := assignmentReservationPaths(details, false); err != nil || paths != nil {
		t.Fatalf("reservations off: paths=%v err=%v, want nothing", paths, err)
	}

	// No path anywhere: refuse before the bead is claimed, with a next step.
	empty := &bv.BeadAssignmentDetails{ID: "ntm-empty", Title: "Think about architecture", Description: "Consider how it should evolve."}
	if _, err := assignmentReservationPaths(empty, true); !errors.Is(err, errNoReservationPaths) {
		t.Fatalf("pathless bead: err = %v, want errNoReservationPaths", err)
	}
}

func TestCLIReservationPortDiscoversDescriptionPaths(t *testing.T) {
	previous := getBeadAssignmentDetailsForAssignment
	getBeadAssignmentDetailsForAssignment = func(_ context.Context, _ string, beadID string) (*bv.BeadAssignmentDetails, error) {
		return &bv.BeadAssignmentDetails{ID: beadID, Title: "Inventory modules and entry points", Description: gh336Description}, nil
	}
	t.Cleanup(func() { getBeadAssignmentDetailsForAssignment = previous })

	stub := newMailStub(t, nil)
	defer stub.Close()
	port := &cliAtomicReservationPort{
		manager: assignpkg.NewFileReservationManager(
			agentmail.NewClient(agentmail.WithBaseURL(stub.server.URL+"/")), "/test/project",
		),
		projectDir: "/test/project",
	}
	request := assignment.ReservationRequest{
		BeadID: "ntm-336", BeadTitle: "Inventory modules and entry points", AgentName: "BlueLake", Target: "%42",
	}

	_, _ = port.Reserve(t.Context(), request)
	if len(stub.reserveCalls) != 1 {
		t.Fatalf("reserve calls = %+v, want one", stub.reserveCalls)
	}
	got := append([]string(nil), stub.reserveCalls[0].Paths...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, gh336Outputs) {
		t.Fatalf("reserved %v, want the description's owned outputs %v", got, gh336Outputs)
	}

	// Reconciliation must look for the same path set the reservation used:
	// both owned outputs held means reserved.
	expires := agentmail.FlexTime{Time: time.Now().UTC().Add(time.Hour)}
	stub.reservations = []agentmail.FileReservation{
		{ID: 71, ProjectID: 1, AgentName: "BlueLake", PathPattern: gh336Outputs[0], Exclusive: true, Reason: "bead assignment: ntm-336", ExpiresTS: expires},
		{ID: 72, ProjectID: 1, AgentName: "BlueLake", PathPattern: gh336Outputs[1], Exclusive: true, Reason: "bead assignment: ntm-336", ExpiresTS: expires},
	}
	recon, err := port.ReconcileReservation(t.Context(), request, assignment.LeaseReceipt{})
	if err != nil {
		t.Fatalf("ReconcileReservation: %v", err)
	}
	if recon.State != assignment.ReservationReconciliationReserved || !reflect.DeepEqual(recon.Lease.ReservationIDs, []int{71, 72}) {
		t.Fatalf("ReconcileReservation = %+v, want reserved with IDs [71 72]", recon)
	}
}
