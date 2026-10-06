package cass

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// TimelineResponse is `cass timeline --json`: the sessions started in Range,
// bucketed under hour or day labels in Groups, or listed flat in Sessions with
// --group-by none.
type TimelineResponse struct {
	Range         TimelineRange                `json:"range"`
	TotalSessions int                          `json:"total_sessions"`
	Groups        map[string][]TimelineSession `json:"groups,omitempty"`
	Sessions      []TimelineSession            `json:"sessions,omitempty"`
}

// TimelineRange bounds a timeline in Unix milliseconds.
type TimelineRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// TimelineSession is one indexed agent session in a timeline.
type TimelineSession struct {
	ID              int64  `json:"id"`
	Agent           string `json:"agent"`
	Title           string `json:"title"`
	StartedAt       int64  `json:"started_at"` // Unix milliseconds
	EndedAt         int64  `json:"ended_at"`
	DurationSeconds int64  `json:"duration_seconds,omitempty"`
	SourcePath      string `json:"source_path"`
	MessageCount    int    `json:"message_count"`
	SourceID        string `json:"source_id,omitempty"`
}

// StartTime returns when the session started.
func (s TimelineSession) StartTime() time.Time {
	return time.UnixMilli(s.StartedAt)
}

// Timeline fetches indexed agent sessions over time. groupBy is hour, day or
// none; agents, when given, restrict the timeline to those agents.
func (c *Client) Timeline(ctx context.Context, since, groupBy string, agents ...string) (*TimelineResponse, error) {
	if !c.IsInstalled() {
		return nil, ErrNotInstalled
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Build arguments for: cass timeline --json [flags]
	args := []string{"timeline", "--json"}
	if since != "" {
		args = append(args, fmt.Sprintf("--since=%s", since))
	}
	if groupBy != "" {
		args = append(args, fmt.Sprintf("--group-by=%s", groupBy))
	}
	for _, agent := range agents {
		if agent != "" {
			args = append(args, fmt.Sprintf("--agent=%s", agent))
		}
	}

	output, err := c.executor.Run(ctx, args...)
	if err != nil {
		return nil, err
	}

	var response TimelineResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("failed to parse timeline response: %w", err)
	}

	return &response, nil
}
