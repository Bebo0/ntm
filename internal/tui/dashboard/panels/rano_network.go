package panels

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Dicklesworthstone/ntm/internal/tui/components"
	"github.com/Dicklesworthstone/ntm/internal/tui/theme"
)

// RanoNetworkRow is one agent pane's recorded connection activity. rano records
// socket connect/close events per process, not HTTP requests or bytes, so the
// panel shows connection counts and recency only.
type RanoNetworkRow struct {
	Label          string
	AgentType      string
	Connections    int
	LastConnection time.Time
	Providers      map[string]int // connections by rano's provider tag
}

// RanoNetworkPanelData holds the data for the rano network activity panel.
type RanoNetworkPanelData struct {
	Loaded    bool
	Enabled   bool
	Available bool
	Version   string

	// PollInterval is used for activity bucketing (best-effort).
	PollInterval time.Duration
	// Window is how far back connections are counted.
	Window time.Duration

	Rows []RanoNetworkRow

	TotalConnections int

	Error error
}

// RanoNetworkPanel displays per-agent connection activity recorded by rano.
type RanoNetworkPanel struct {
	PanelBase
	data  RanoNetworkPanelData
	theme theme.Theme
}

func ranoNetworkConfig() PanelConfig {
	return PanelConfig{
		ID:              "rano_network",
		Title:           "Network Activity",
		Priority:        PriorityNormal,
		RefreshInterval: 1 * time.Second,
		MinWidth:        30,
		MinHeight:       8,
		Collapsible:     true,
	}
}

func NewRanoNetworkPanel() *RanoNetworkPanel {
	return &RanoNetworkPanel{
		PanelBase: NewPanelBase(ranoNetworkConfig()),
		theme:     theme.Current(),
	}
}

func (p *RanoNetworkPanel) Init() tea.Cmd { return nil }

func (p *RanoNetworkPanel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { return p, nil }

func (p *RanoNetworkPanel) SetData(data RanoNetworkPanelData) {
	p.data = data
	if data.Error == nil && data.Loaded {
		p.SetLastUpdate(time.Now())
	}
}

func (p *RanoNetworkPanel) HasData() bool {
	return p.data.Loaded || p.data.Error != nil
}

func (p *RanoNetworkPanel) View() string {
	t := p.theme
	w, h := p.Width(), p.Height()
	if w <= 0 || h <= 0 {
		return ""
	}

	borderColor := t.Surface1
	bgColor := t.Base
	if p.IsFocused() {
		borderColor = t.Primary
		bgColor = t.Surface0
	} else if p.data.Available && len(p.data.Rows) > 0 {
		borderColor = t.Green
	}

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Background(bgColor).
		Width(w-2).
		Height(h-2).
		Padding(0, 1)

	var content strings.Builder

	title := lipgloss.NewStyle().Bold(true).Foreground(t.Text).Render("Network Activity")
	if p.data.Version != "" {
		title = fmt.Sprintf("%s %s%s%s", title, "\033[2m", p.data.Version, "\033[0m")
	}
	content.WriteString(title + "\n")

	if p.data.Error != nil {
		content.WriteString("\n" + components.RenderErrorState(components.ErrorStateOptions{
			Title:       "Network stats unavailable",
			Description: p.data.Error.Error(),
			Width:       w - 4,
		}))
		return boxStyle.Render(FitToHeight(content.String(), h-4))
	}

	if !p.data.Enabled {
		content.WriteString("\n" + components.RenderEmptyState(components.EmptyStateOptions{
			Icon:        components.IconExternal,
			Title:       "rano disabled",
			Description: "Enable integrations.rano in config",
			Width:       w - 4,
			Centered:    true,
		}))
		return boxStyle.Render(FitToHeight(content.String(), h-4))
	}

	if !p.data.Available {
		content.WriteString("\n" + components.RenderEmptyState(components.EmptyStateOptions{
			Icon:        components.IconWaiting,
			Title:       "rano not available",
			Description: "Install rano and make sure `rano status` succeeds",
			Width:       w - 4,
			Centered:    true,
		}))
		return boxStyle.Render(FitToHeight(content.String(), h-4))
	}

	if len(p.data.Rows) == 0 {
		content.WriteString("\n" + components.RenderEmptyState(components.EmptyStateOptions{
			Icon:        components.IconWaiting,
			Title:       "No agent connections",
			Description: "No connections recorded " + ranoWindowLabel(p.data.Window),
			Width:       w - 4,
			Centered:    true,
		}))
		return boxStyle.Render(FitToHeight(content.String(), h-4))
	}

	rows := append([]RanoNetworkRow(nil), p.data.Rows...)
	sort.Slice(rows, func(i, j int) bool {
		// Most-recent connection first; fall back to more connections.
		if !rows[i].LastConnection.Equal(rows[j].LastConnection) {
			return rows[i].LastConnection.After(rows[j].LastConnection)
		}
		return rows[i].Connections > rows[j].Connections
	})

	expanded := h >= 14

	content.WriteString("\n")
	content.WriteString(renderRanoTable(t, w-4, rows, p.data.PollInterval))
	if expanded {
		content.WriteString("\n")
		content.WriteString(fmt.Sprintf("Total: %d conn %s\n", p.data.TotalConnections, ranoWindowLabel(p.data.Window)))
		content.WriteString(renderRanoProviderBreakdown(t, w-4, rows))
	}

	return boxStyle.Render(FitToHeight(content.String(), h-4))
}

func ranoWindowLabel(window time.Duration) string {
	switch {
	case window <= 0:
		return "recently"
	case window%time.Hour == 0:
		return fmt.Sprintf("in the last %dh", window/time.Hour)
	case window%time.Minute == 0:
		return fmt.Sprintf("in the last %dm", window/time.Minute)
	default:
		return "in the last " + window.String()
	}
}

func renderRanoTable(t theme.Theme, width int, rows []RanoNetworkRow, pollInterval time.Duration) string {
	if width <= 0 {
		return ""
	}

	// Columns: Agent | Conn | Last | Activity
	// Keep this simple and stable; don't try to fully auto-fit.
	connW := 5
	lastW := 6
	actW := 9
	sep := "  "

	agentW := width - (connW + lastW + actW + len(sep)*3)
	if agentW < 10 {
		agentW = 10
	}

	header := fmt.Sprintf("%-*s%s%*s%s%*s%s%-*s",
		agentW, "Agent",
		sep, connW, "Conn",
		sep, lastW, "Last",
		sep, actW, "Activity",
	)
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Foreground(t.Subtext).Render(header) + "\n")

	for _, row := range rows {
		label := row.Label
		if label == "" {
			label = "(unknown)"
		}
		label = truncateWidth(label, agentW)

		line := fmt.Sprintf("%-*s%s%*d%s%*s%s%-*s",
			agentW, label,
			sep, connW, row.Connections,
			sep, lastW, formatConnectionAge(row.LastConnection),
			sep, actW, renderActivity(row.LastConnection, pollInterval),
		)
		b.WriteString(line + "\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// renderRanoProviderBreakdown sums connections by rano's own provider tag
// (anthropic, openai, google, unknown), as recorded per connection.
func renderRanoProviderBreakdown(t theme.Theme, width int, rows []RanoNetworkRow) string {
	byProvider := make(map[string]int)
	for _, row := range rows {
		for provider, count := range row.Providers {
			if count > 0 {
				byProvider[provider] += count
			}
		}
	}

	// rano's labels in its own order; any other tag sorts after them.
	rank := map[string]int{"anthropic": 0, "openai": 1, "google": 2, "unknown": 3}
	providers := make([]string, 0, len(byProvider))
	for provider := range byProvider {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool {
		ri, iKnown := rank[providers[i]]
		rj, jKnown := rank[providers[j]]
		if iKnown != jKnown {
			return iKnown
		}
		if iKnown && ri != rj {
			return ri < rj
		}
		return providers[i] < providers[j]
	})

	var parts []string
	for _, provider := range providers {
		parts = append(parts, fmt.Sprintf("%s: %d", provider, byProvider[provider]))
	}
	if len(parts) == 0 {
		return ""
	}
	prefix := lipgloss.NewStyle().Foreground(t.Subtext).Render("By provider: ")
	line := prefix + strings.Join(parts, "  ")
	return truncateWidth(line, width) + "\n"
}

// formatConnectionAge renders how long ago the last connection was recorded.
func formatConnectionAge(last time.Time) string {
	if last.IsZero() {
		return "-"
	}
	age := time.Since(last)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds", max(int(age/time.Second), 0))
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age/time.Minute))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
	}
}

func renderActivity(last time.Time, pollInterval time.Duration) string {
	if last.IsZero() {
		return "(idle)"
	}
	age := time.Since(last)
	if pollInterval <= 0 {
		pollInterval = 1 * time.Second
	}
	switch {
	case age <= pollInterval:
		return "▲▲▲"
	case age <= 5*pollInterval:
		return "▲▲"
	case age <= 30*pollInterval:
		return "▲"
	default:
		return "(idle)"
	}
}

func truncateWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	// Reserve space for ellipsis.
	if w <= 3 {
		return s[:w]
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}
