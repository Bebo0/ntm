package templates

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse_WithFrontmatter(t *testing.T) {
	content := `---
name: test_template
description: A test template
variables:
  - name: file
    description: File to review
    required: true
  - name: focus
    description: Area to focus on
---
Review the following:
{{file}}

{{#focus}}
Focus on: {{focus}}
{{/focus}}`

	tmpl, err := Parse(content)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if tmpl.Name != "test_template" {
		t.Errorf("Name = %q, want %q", tmpl.Name, "test_template")
	}

	if tmpl.Description != "A test template" {
		t.Errorf("Description = %q, want %q", tmpl.Description, "A test template")
	}

	if len(tmpl.Variables) != 2 {
		t.Errorf("len(Variables) = %d, want 2", len(tmpl.Variables))
	}

	if !tmpl.Variables[0].Required {
		t.Error("First variable should be required")
	}

	if tmpl.Variables[1].Required {
		t.Error("Second variable should not be required")
	}
}

func TestParse_NoFrontmatter(t *testing.T) {
	content := `Just a simple template with {{variable}} placeholders.`

	tmpl, err := Parse(content)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if tmpl.Name != "" {
		t.Errorf("Name = %q, want empty", tmpl.Name)
	}

	if tmpl.Body != content {
		t.Errorf("Body = %q, want %q", tmpl.Body, content)
	}
}

func TestSubstituteVariables(t *testing.T) {
	tests := []struct {
		name string
		body string
		vars map[string]string
		want string
	}{
		{
			name: "simple substitution",
			body: "Hello {{name}}!",
			vars: map[string]string{"name": "World"},
			want: "Hello World!",
		},
		{
			name: "multiple variables",
			body: "{{greeting}}, {{name}}!",
			vars: map[string]string{"greeting": "Hi", "name": "Alice"},
			want: "Hi, Alice!",
		},
		{
			name: "unmatched variable",
			body: "Hello {{name}}!",
			vars: map[string]string{},
			want: "Hello {{name}}!",
		},
		{
			name: "variable in middle",
			body: "The {{color}} fox jumps.",
			vars: map[string]string{"color": "brown"},
			want: "The brown fox jumps.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := substituteVariables(tt.body, tt.vars)
			if got != tt.want {
				t.Errorf("substituteVariables() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpandConditionals(t *testing.T) {
	tests := []struct {
		name string
		body string
		vars map[string]string
		want string
	}{
		{
			name: "conditional included",
			body: "Start {{#focus}}Focus: here{{/focus}} End",
			vars: map[string]string{"focus": "security"},
			want: "Start Focus: here End",
		},
		{
			name: "conditional excluded",
			body: "Start {{#focus}}Focus: something{{/focus}} End",
			vars: map[string]string{},
			want: "Start  End",
		},
		{
			name: "conditional with empty value",
			body: "Start {{#focus}}Focus: something{{/focus}} End",
			vars: map[string]string{"focus": ""},
			want: "Start  End",
		},
		{
			name: "nested conditionals",
			body: "{{#a}}A{{#b}}B{{/b}}{{/a}}",
			vars: map[string]string{"a": "1", "b": "2"},
			want: "AB",
		},
		{
			name: "multiple conditionals",
			body: "{{#one}}1{{/one}} {{#two}}2{{/two}}",
			vars: map[string]string{"one": "yes"},
			want: "1 ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandConditionals(tt.body, tt.vars)
			if got != tt.want {
				t.Errorf("expandConditionals() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTemplate_Execute(t *testing.T) {
	tmpl := &Template{
		Name: "test",
		Variables: []VariableSpec{
			{Name: "file", Required: true},
			{Name: "focus", Required: false},
		},
		Body: `Review this code:
{{file}}

{{#focus}}
Focus on: {{focus}}
{{/focus}}`,
	}

	t.Run("with all variables", func(t *testing.T) {
		ctx := ExecutionContext{
			Variables: map[string]string{
				"file":  "func main() {}",
				"focus": "security",
			},
			FileContent: "func main() {}",
		}

		result, err := tmpl.Execute(ctx)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if !contains(result, "func main()") {
			t.Error("Result should contain file content")
		}

		if !contains(result, "Focus on: security") {
			t.Error("Result should contain focus section")
		}
	})

	t.Run("without optional variable", func(t *testing.T) {
		ctx := ExecutionContext{
			Variables: map[string]string{
				"file": "func main() {}",
			},
			FileContent: "func main() {}",
		}

		result, err := tmpl.Execute(ctx)
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}

		if contains(result, "Focus on:") {
			t.Error("Result should not contain focus section")
		}
	})

	t.Run("missing required variable", func(t *testing.T) {
		ctx := ExecutionContext{
			Variables: map[string]string{},
		}

		_, err := tmpl.Execute(ctx)
		if err == nil {
			t.Error("Execute should fail with missing required variable")
		}
	})
}

func TestTemplate_ExecuteRefusesUnresolvedPlaceholders(t *testing.T) {
	tmpl := &Template{
		Name: "roll_call",
		Body: "Agent #{{agent_num}} ({{AGENT_TYPE}}) on {{BEAD_ID}}: {{TITLE}} via {{channel}}{{#extra}} {{extra}}{{/extra}}",
	}

	t.Run("lists every unresolved name canonically", func(t *testing.T) {
		_, err := tmpl.Execute(ExecutionContext{})
		var unresolved *UnresolvedVariablesError
		if !errors.As(err, &unresolved) {
			t.Fatalf("Execute error = %v, want *UnresolvedVariablesError", err)
		}
		want := []string{"agent_num", "agent_type", "bead_id", "bead_title", "channel"}
		if !reflect.DeepEqual(unresolved.Names, want) {
			t.Fatalf("unresolved names = %v, want %v (aliases folded, sorted, conditional body skipped)", unresolved.Names, want)
		}
		if unresolved.Template != "roll_call" || !strings.Contains(err.Error(), `template "roll_call" has unresolved variable(s): agent_num, agent_type, bead_id, bead_title, channel`) {
			t.Fatalf("error message = %q", err.Error())
		}
	})

	t.Run("full context renders with aliases", func(t *testing.T) {
		ctx := ExecutionContext{Variables: map[string]string{"channel": "mail"}}.
			WithBead("bd-7", "Fix it", "", "", "", "").
			WithAgent(3, "codex", "", "%12")
		got, err := tmpl.Execute(ctx)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if want := "Agent #3 (codex) on bd-7: Fix it via mail"; got != want {
			t.Fatalf("Execute = %q, want %q", got, want)
		}
	})

	t.Run("user-supplied canonical variable fills its alias", func(t *testing.T) {
		ctx := ExecutionContext{Variables: map[string]string{
			"channel": "mail", "bead_id": "bd-9", "bead_title": "Typed by hand",
			"agent_num": "1", "agent_type": "claude",
		}}
		got, err := tmpl.Execute(ctx)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if want := "Agent #1 (claude) on bd-9: Typed by hand via mail"; got != want {
			t.Fatalf("Execute = %q, want %q", got, want)
		}
	})

	t.Run("substituted values are neither checked nor re-expanded", func(t *testing.T) {
		fileTmpl := &Template{Name: "review", Body: "Review:\n{{file}}"}
		got, err := fileTmpl.Execute(ExecutionContext{FileContent: "<p>{{user_name}}</p>"})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if want := "Review:\n<p>{{user_name}}</p>"; got != want {
			t.Fatalf("Execute = %q, want %q", got, want)
		}
	})
}

func TestTemplate_ExecuteSharedDefersOnlyPaneContext(t *testing.T) {
	tmpl := &Template{
		Name: "assignment",
		Body: "{{send_num}}/{{send_total}} Agent #{{AGENT_NUM}} ({{agent_type}}{{#agent_variant}}:{{agent_variant}}{{/agent_variant}}) pane {{agent_pane}} index {{send_index}}: {{bead_id}}",
	}

	shared, err := tmpl.ExecuteShared(ExecutionContext{}.WithBead("bd-3", "T", "", "", "", ""))
	if err != nil {
		t.Fatalf("ExecuteShared with bead context: %v", err)
	}
	if want := "{{send_num}}/{{send_total}} Agent #{{AGENT_NUM}} ({{agent_type}}) pane {{agent_pane}} index {{send_index}}: bd-3"; shared != want {
		t.Fatalf("ExecuteShared = %q, want pane placeholders kept and bead filled: %q", shared, want)
	}

	_, err = tmpl.ExecuteShared(ExecutionContext{})
	var unresolved *UnresolvedVariablesError
	if !errors.As(err, &unresolved) || !reflect.DeepEqual(unresolved.Names, []string{"bead_id"}) {
		t.Fatalf("ExecuteShared without bead error = %v, want only bead_id unresolved", err)
	}

	_, err = tmpl.Execute(ExecutionContext{}.WithBead("bd-3", "T", "", "", "", ""))
	if !errors.As(err, &unresolved) || !reflect.DeepEqual(unresolved.Names, []string{"agent_num", "agent_pane", "agent_type", "send_index", "send_num", "send_total"}) {
		t.Fatalf("Execute without pane context error = %v, want every pane variable unresolved", err)
	}

	perPane, err := tmpl.Execute(ExecutionContext{}.
		WithBead("bd-3", "T", "", "", "", "").
		WithAgent(2, "claude", "opus", "%5").
		WithSendBatch(1, 4))
	if err != nil {
		t.Fatalf("Execute with pane context: %v", err)
	}
	if want := "2/4 Agent #2 (claude:opus) pane %5 index 1: bd-3"; perPane != want {
		t.Fatalf("Execute = %q, want %q", perPane, want)
	}
}

func TestLoader_Load_ReturnsParseError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	projectDir := filepath.Join(tmp, "project")
	templateDir := filepath.Join(projectDir, ".ntm", "templates")
	if err := os.MkdirAll(templateDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	name := "bad_parse_template"
	// Invalid YAML frontmatter: unclosed sequence.
	content := "---\nname: [\n---\nHello\n"
	if err := os.WriteFile(filepath.Join(templateDir, name+".md"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loader := newLoaderWithProjectForTest(projectDir)
	_, err := loader.Load(name)
	if err == nil {
		t.Fatalf("Load(%q) expected error, got nil", name)
	}

	var notFound *TemplateNotFoundError
	if errors.As(err, &notFound) {
		t.Fatalf("Load(%q) returned TemplateNotFoundError; want parse error: %v", name, err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// =============================================================================
// TemplateSource.String (bd-8gkp7)
// =============================================================================

func TestTemplateSource_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		source TemplateSource
		want   string
	}{
		{SourceBuiltin, "builtin"},
		{SourceUser, "user"},
		{SourceProject, "project"},
		{TemplateSource(99), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := tt.source.String(); got != tt.want {
				t.Errorf("TemplateSource(%d).String() = %q, want %q", tt.source, got, tt.want)
			}
		})
	}
}

// =============================================================================
// Template.HasVariable (bd-8gkp7)
// =============================================================================

func TestTemplate_HasVariable(t *testing.T) {
	t.Parallel()
	tmpl := &Template{
		Variables: []VariableSpec{
			{Name: "file", Required: true},
			{Name: "session", Required: false},
		},
	}

	if !tmpl.HasVariable("file") {
		t.Error("HasVariable(file) should return true")
	}
	if !tmpl.HasVariable("session") {
		t.Error("HasVariable(session) should return true")
	}
	if tmpl.HasVariable("nonexistent") {
		t.Error("HasVariable(nonexistent) should return false")
	}
}

func TestTemplate_HasVariable_Empty(t *testing.T) {
	t.Parallel()
	tmpl := &Template{}
	if tmpl.HasVariable("anything") {
		t.Error("HasVariable should return false for template with no variables")
	}
}

func TestParse_WithUnknownFrontmatterField(t *testing.T) {
	content := `---
name: strict_template
legacy: true
---
Hello`

	_, err := Parse(content)
	if err == nil {
		t.Fatal("expected error for unknown frontmatter field")
	}
	if !contains(err.Error(), "field legacy not found") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestLoader_Load_ReturnsUnknownFrontmatterError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	projectDir := filepath.Join(tmp, "project")
	templateDir := filepath.Join(projectDir, ".ntm", "templates")
	if err := os.MkdirAll(templateDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	name := "bad_unknown_field_template"
	content := `---
name: bad_template
legacy: true
---
Hello
`
	if err := os.WriteFile(filepath.Join(templateDir, name+".md"), []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	loader := newLoaderWithProjectForTest(projectDir)
	_, err := loader.Load(name)
	if err == nil {
		t.Fatalf("Load(%q) expected error, got nil", name)
	}

	var notFound *TemplateNotFoundError
	if errors.As(err, &notFound) {
		t.Fatalf("Load(%q) returned TemplateNotFoundError; want parse error: %v", name, err)
	}
	if !contains(err.Error(), "field legacy not found") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

// newLoaderWithProjectForTest builds a Loader rooted at a test project dir.
func newLoaderWithProjectForTest(projectPath string) *Loader {
	projectDir := resolveProjectTemplateDir(projectPath, projectPath)
	return &Loader{
		projectDir: projectDir,
		userDir:    getDefaultUserTemplateDir(),
	}
}
