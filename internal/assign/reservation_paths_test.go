package assign

import (
	"reflect"
	"sort"
	"testing"
)

// The bead description from ntm#336, generalized: inputs in a prose
// paragraph, outputs as backticked list items under an ownership heading.
const issue336Description = "Read README.md and docs/research-status.md.\n\n" +
	"## Owned outputs\n\n" +
	"- `docs/specification/work/inventory.md`\n" +
	"- `docs/evidence/inventory.md`\n\n" +
	"## Acceptance criteria\n\n" +
	"- Inventory the modules and cite evidence. Re-run tests/inventory_test.go.\n"

func sortedPaths(paths []string) []string {
	out := append([]string(nil), paths...)
	sort.Strings(out)
	return out
}

func TestReservationPathsForBeadUsesDeclaredOutputs(t *testing.T) {
	got := ReservationPathsForBead("Inventory modules and entry points", issue336Description)
	want := []string{"docs/evidence/inventory.md", "docs/specification/work/inventory.md"}
	if !reflect.DeepEqual(sortedPaths(got), want) {
		t.Fatalf("ReservationPathsForBead() = %v, want exactly the owned outputs %v", got, want)
	}
}

func TestReservationPathsForBeadSectionStyles(t *testing.T) {
	for _, tc := range []struct {
		name        string
		description string
		want        []string
	}{
		{
			name:        "label line",
			description: "Context in internal/old/legacy.go.\n\nFiles to modify:\n- internal/cli/assign.go\n- internal/bv/bv.go\n\nNotes:\n- see internal/old/other.go\n",
			want:        []string{"internal/bv/bv.go", "internal/cli/assign.go"},
		},
		{
			name:        "bold label",
			description: "Uses pkg/input.go.\n\n**Owned files**\n- `pkg/output.go`\n",
			want:        []string{"pkg/output.go"},
		},
		{
			name:        "two owned sections",
			description: "### Outputs\n- a/one.go\n### Inputs\n- a/in.go\n### Owned paths\n- a/two.go\n",
			want:        []string{"a/one.go", "a/two.go"},
		},
		{
			name:        "no declared section uses title and description",
			description: "Update `internal/api/server.go` and internal/api/routes.go",
			want:        []string{"internal/api/routes.go", "internal/api/server.go", "internal/title.go"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title := "Refactor"
			if tc.name == "no declared section uses title and description" {
				title = "Refactor internal/title.go"
			}
			if got := sortedPaths(ReservationPathsForBead(title, tc.description)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ReservationPathsForBead() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReservationPathsForBeadDeclaredButEmptyFailsClosed(t *testing.T) {
	description := "Read internal/cli/assign.go.\n\n## Owned outputs\n\nTo be decided.\n"
	if got := ReservationPathsForBead("Fix internal/cli/assign.go", description); len(got) != 0 {
		t.Fatalf("ReservationPathsForBead() = %v; a declared ownership section with no paths must not fall back to inputs", got)
	}
}

func TestExtractFilePathsBacktickedAndNoFilenamePrefixGlobs(t *testing.T) {
	got := ExtractFilePaths("", "Read README.md and docs/research-status.md, then edit `docs/specification/work/inventory.md` and `.github/ci.yml`.")
	for _, want := range []string{"README.md", "docs/research-status.md", "docs/specification/work/inventory.md", ".github/ci.yml"} {
		found := false
		for _, p := range got {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	for _, p := range got {
		switch p {
		case "docs/research-status/**/*", "docs/specification/work/inventory/**/*":
			t.Errorf("file name became a directory glob: %q (all: %v)", p, got)
		}
	}

	// A directory named on its own still widens to a glob, including at the
	// end of a sentence and inside backticks.
	dirs := ExtractFilePaths("", "Move commands to internal/cli. Touch `pkg/api` too.")
	for _, want := range []string{"internal/cli/**/*", "pkg/api/**/*"} {
		found := false
		for _, p := range dirs {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing directory glob %q in %v", want, dirs)
		}
	}
}
