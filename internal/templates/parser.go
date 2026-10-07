package templates

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Pre-compiled regex patterns for variable substitution
var (
	// simpleVarRe matches simple {{variable}} placeholders
	simpleVarRe = regexp.MustCompile(`\{\{([a-zA-Z_][a-zA-Z0-9_]*)\}\}`)
	// conditionalOpenRe matches conditional opening tags {{#variable}}
	conditionalOpenRe = regexp.MustCompile(`\{\{#([a-zA-Z_][a-zA-Z0-9_]*)\}\}`)
)

// Parse parses a template from markdown content with YAML frontmatter.
// Format:
//
//	---
//	name: template_name
//	description: What this template does
//	variables:
//	  - name: file
//	    description: File path to review
//	    required: true
//	---
//	The template body with {{variable}} placeholders.
func Parse(content string) (*Template, error) {
	tmpl := &Template{}

	// Check for frontmatter
	if strings.HasPrefix(content, "---") {
		parts := strings.SplitN(content, "---", 3)
		if len(parts) >= 3 {
			// Parse YAML frontmatter strictly so typos fail loudly.
			frontmatter := []byte(parts[1])
			if len(bytes.TrimSpace(frontmatter)) > 0 {
				dec := yaml.NewDecoder(bytes.NewReader(frontmatter))
				dec.KnownFields(true)
				if err := dec.Decode(tmpl); err != nil && err != io.EOF {
					return nil, err
				}
			}
			tmpl.Body = strings.TrimSpace(parts[2])
		} else {
			// No valid frontmatter, treat entire content as body
			tmpl.Body = strings.TrimSpace(content)
		}
	} else {
		// No frontmatter, entire content is body
		tmpl.Body = strings.TrimSpace(content)
	}

	return tmpl, nil
}

// paneContextVariables are filled per target pane at send time (WithAgent,
// WithSendBatch), including their uppercase aliases.
var paneContextVariables = map[string]struct{}{
	"agent_num":     {},
	"AGENT_NUM":     {},
	"agent_type":    {},
	"AGENT_TYPE":    {},
	"agent_variant": {},
	"VARIANT":       {},
	"agent_pane":    {},
	"send_index":    {},
	"send_total":    {},
	"send_num":      {},
}

// contextAliases maps the uppercase convenience spellings templates may use to
// their canonical variable. The alias always mirrors the canonical value, so a
// value supplied either way (--var bead_id=..., or bead context) fills both.
var contextAliases = map[string]string{
	"BEAD_ID":     "bead_id",
	"TITLE":       "bead_title",
	"PRIORITY":    "bead_priority",
	"DESCRIPTION": "bead_description",
	"AGENT_NUM":   "agent_num",
	"AGENT_TYPE":  "agent_type",
	"VARIANT":     "agent_variant",
}

// UnresolvedVariablesError reports template placeholders that no variable,
// default, builtin, or execution context filled. Delivering the literal
// "{{name}}" text to an agent is never correct, so rendering refuses instead.
type UnresolvedVariablesError struct {
	Template string
	// Names are canonical variable names (aliases folded), sorted.
	Names []string
}

func (e *UnresolvedVariablesError) Error() string {
	return fmt.Sprintf("template %q has unresolved variable(s): %s", e.Template, strings.Join(e.Names, ", "))
}

// Execute renders the template for one fully known context. A placeholder in
// the template body that nothing fills is an *UnresolvedVariablesError.
func (t *Template) Execute(ctx ExecutionContext) (string, error) {
	return t.execute(ctx, false)
}

// ExecuteShared renders everything known before send targets are resolved.
// Per-pane placeholders ({{agent_num}}, {{agent_type}}, {{send_num}}, ...)
// stay literal for the per-target Execute; any other unresolved placeholder is
// an *UnresolvedVariablesError, so missing bead context fails before a send
// touches any pane.
func (t *Template) ExecuteShared(ctx ExecutionContext) (string, error) {
	return t.execute(ctx, true)
}

func (t *Template) execute(ctx ExecutionContext, deferPaneContext bool) (string, error) {
	// Validate required variables
	if err := t.Validate(ctx); err != nil {
		return "", err
	}

	vars := t.variables(ctx)

	// First, expand conditionals {{#var}}...{{/var}}
	body := expandConditionals(t.Body, vars)

	// Refuse placeholders nothing fills. Only the template body is checked:
	// substituted values (file content, bead descriptions) may legitimately
	// contain "{{...}}" text and are never re-expanded.
	if missing := unresolvedVariables(body, vars, deferPaneContext); len(missing) > 0 {
		return "", &UnresolvedVariablesError{Template: t.Name, Names: missing}
	}

	// Then, substitute simple variables {{var}}
	return substituteVariables(body, vars), nil
}

// variables builds the substitution map:
// defaults < builtins < user vars < special/context vars, then aliases.
func (t *Template) variables(ctx ExecutionContext) map[string]string {
	vars := make(map[string]string)

	// Apply defaults from template definition
	for _, v := range t.Variables {
		if v.Default != "" {
			vars[v.Name] = v.Default
		}
	}

	// Apply builtin variables
	for k, v := range BuiltinVariables() {
		vars[k] = v
	}

	// Apply user-provided variables
	for k, v := range ctx.Variables {
		vars[k] = v
	}

	// Apply special context variables
	if ctx.FileContent != "" {
		vars["file"] = ctx.FileContent
	}
	if ctx.Session != "" {
		vars["session"] = ctx.Session
	}
	if ctx.Clipboard != "" {
		vars["clipboard"] = ctx.Clipboard
	}

	// Apply bead context variables
	if ctx.BeadID != "" {
		vars["bead_id"] = ctx.BeadID
	}
	if ctx.BeadTitle != "" {
		vars["bead_title"] = ctx.BeadTitle
	}
	if ctx.BeadPriority != "" {
		vars["bead_priority"] = ctx.BeadPriority
	}
	if ctx.BeadDescription != "" {
		vars["bead_description"] = ctx.BeadDescription
	}
	if ctx.BeadStatus != "" {
		vars["bead_status"] = ctx.BeadStatus
	}
	if ctx.BeadType != "" {
		vars["bead_type"] = ctx.BeadType
	}

	// Apply agent context variables
	if ctx.AgentNum > 0 {
		vars["agent_num"] = strconv.Itoa(ctx.AgentNum)
	}
	if ctx.AgentType != "" {
		vars["agent_type"] = ctx.AgentType
	}
	if ctx.AgentVariant != "" {
		vars["agent_variant"] = ctx.AgentVariant
	}
	if ctx.AgentPane != "" {
		vars["agent_pane"] = ctx.AgentPane
	}

	// Apply send batch context variables
	if ctx.SendTotal > 0 {
		vars["send_index"] = strconv.Itoa(ctx.SendIndex)
		vars["send_total"] = strconv.Itoa(ctx.SendTotal)
		vars["send_num"] = strconv.Itoa(ctx.SendIndex + 1) // 1-indexed for human readability
	}

	// Mirror canonical values into their uppercase aliases.
	for alias, canonical := range contextAliases {
		if value, ok := vars[canonical]; ok {
			vars[alias] = value
		}
	}

	return vars
}

// unresolvedVariables lists the canonical names of {{placeholders}} in body
// that vars does not fill, sorted and de-duplicated. With deferPaneContext,
// per-pane variables are left for the per-target render.
func unresolvedVariables(body string, vars map[string]string, deferPaneContext bool) []string {
	seen := make(map[string]struct{})
	var missing []string
	for _, match := range simpleVarRe.FindAllStringSubmatch(body, -1) {
		name := match[1]
		if _, ok := vars[name]; ok {
			continue
		}
		if _, pane := paneContextVariables[name]; pane && deferPaneContext {
			continue
		}
		if canonical, ok := contextAliases[name]; ok {
			name = canonical
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return missing
}

// substituteVariables replaces {{variable}} placeholders with values.
// Note: The regex only matches simple variables like {{foo}}, not conditional
// markers like {{#var}} or {{/var}} (which don't start with [a-zA-Z_]).
func substituteVariables(body string, vars map[string]string) string {
	return simpleVarRe.ReplaceAllStringFunc(body, func(match string) string {
		// Extract variable name
		name := match[2 : len(match)-2]

		if val, ok := vars[name]; ok {
			return val
		}
		return match // Leave unmatched variables as-is
	})
}

// expandConditionals handles {{#variable}}...{{/variable}} blocks.
// If the variable is set and non-empty, the block content is included.
// Otherwise, the entire block is removed.
func expandConditionals(body string, vars map[string]string) string {
	// Process until no more matches (handles nested conditionals)
	for {
		matches := conditionalOpenRe.FindStringSubmatchIndex(body)
		if matches == nil {
			break // No more opening tags
		}

		// Extract variable name
		varName := body[matches[2]:matches[3]]
		openStart := matches[0]
		openEnd := matches[1]

		// Find matching closing tag
		closeTag := "{{/" + varName + "}}"
		closeStart := strings.Index(body[openEnd:], closeTag)
		if closeStart == -1 {
			// No matching close tag, leave as-is and skip
			break
		}
		closeStart += openEnd
		closeEnd := closeStart + len(closeTag)

		// Extract content between tags
		content := body[openEnd:closeStart]

		// Determine replacement
		var replacement string
		if val, ok := vars[varName]; ok && val != "" {
			replacement = content
		}
		// else: replacement is empty string, removing the block

		// Rebuild body
		body = body[:openStart] + replacement + body[closeEnd:]
	}

	return body
}

// macroRe matches @macro-name patterns for inline template expansion.
// Supports both hyphenated names (@marching-orders) and underscored names (@marching_orders).
var macroRe = regexp.MustCompile(`@([a-zA-Z][a-zA-Z0-9_-]*)`)
