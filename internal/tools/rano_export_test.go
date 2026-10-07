package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ranoExportRecordedShape is what upstream rano emits for
// `rano export --format jsonl --sqlite DB --fields ts,event,pid,comm,provider`.
// Source: https://github.com/Dicklesworthstone/rano/blob/7f342a3d195dea97f083602274fd2758ff46c88e/src/main.rs
// (rano 0.2.1): run_export/build_export_query select the fields ORDER BY ts
// ASC; format_jsonl_row writes keys in BTreeMap (alphabetical) order and omits
// NULL columns; system_time_to_rfc3339 stores whole-second UTC "Z" stamps;
// synthesized_alert_event rows carry event "alert", an empty comm and a NULL
// (omitted) pid. There is no `rano stats` subcommand (parse_cli dispatches only
// update, report, export, config, diff and status).
const ranoExportRecordedShape = `{"comm":"claude","event":"connect","pid":4242,"provider":"anthropic","ts":"2026-01-20T12:00:00Z"}
{"comm":"claude","event":"connect","pid":4242,"provider":"anthropic","ts":"2026-01-20T12:00:03Z"}
{"comm":"node","event":"connect","pid":4243,"provider":"unknown","ts":"2026-01-20T12:00:04Z"}
{"comm":"claude","event":"close","pid":4242,"provider":"anthropic","ts":"2026-01-20T12:00:05Z"}
{"comm":"codex","event":"connect","pid":5151,"provider":"openai","ts":"2026-01-20T12:01:00Z"}
{"comm":"","event":"alert","provider":"anthropic","ts":"2026-01-20T12:01:30Z"}
`

func TestRanoExportRecordedUpstreamShape(t *testing.T) {
	since := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	stats, err := aggregateRanoExport([]byte(ranoExportRecordedShape), since, since.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("recorded rano export rejected: %v", err)
	}
	want := []RanoProcessStats{
		{PID: 4242, ProcessName: "claude", ConnectionCount: 2, LastConnection: "2026-01-20T12:00:03Z", Providers: map[string]int{"anthropic": 2}},
		{PID: 4243, ProcessName: "node", ConnectionCount: 1, LastConnection: "2026-01-20T12:00:04Z", Providers: map[string]int{"unknown": 1}},
		{PID: 5151, ProcessName: "codex", ConnectionCount: 1, LastConnection: "2026-01-20T12:01:00Z", Providers: map[string]int{"openai": 1}},
	}
	if len(stats) != len(want) {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	for i := range want {
		got := stats[i]
		if got.PID != want[i].PID || got.ProcessName != want[i].ProcessName || got.ConnectionCount != want[i].ConnectionCount ||
			got.LastConnection != want[i].LastConnection || len(got.Providers) != len(want[i].Providers) {
			t.Fatalf("stats[%d] = %+v, want %+v (close/alert rows must not count)", i, got, want[i])
		}
		for provider, count := range want[i].Providers {
			if got.Providers[provider] != count {
				t.Fatalf("stats[%d].Providers = %v, want %v", i, got.Providers, want[i].Providers)
			}
		}
	}
	// A narrower window keeps only the rows inside it.
	stats, err = aggregateRanoExport([]byte(ranoExportRecordedShape), since.Add(time.Minute), since.Add(5*time.Minute))
	if err != nil || len(stats) != 1 || stats[0].PID != 5151 {
		t.Fatalf("window filter = %+v, %v", stats, err)
	}
}

func TestRanoExportConnectionMeasurements(t *testing.T) {
	since := time.Date(2026, 1, 17, 10, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	data := []byte(`{"ts":"2026-01-17T10:05:00Z","event":"connect","pid":42,"comm":"codex","provider":"openai"}
{"ts":"2026-01-17T10:05:01Z","event":"close","pid":42,"provider":"openai"}
{"ts":"2026-01-17T10:06:00Z","event":"connect","pid":42,"comm":"node","provider":" ANTHROPIC "}
{"ts":"2026-01-17T10:00:00Z","event":"connect","pid":7}
{"ts":"2026-01-17T09:59:59Z","event":"connect","pid":99}
{"ts":"2026-01-17T11:00:00Z","event":"connect","pid":99}
{"ts":"2026-01-17T10:05:00Z","event":"alert"}
{"ts":"2026-01-17T10:05:00Z","event":"connect"}
`)
	stats, err := aggregateRanoExport(data, since, until)
	if err != nil || len(stats) != 2 {
		t.Fatalf("export = %+v, %v", stats, err)
	}
	if stats[0].PID != 7 || stats[0].ConnectionCount != 1 || stats[0].Providers["unknown"] != 1 {
		t.Fatalf("missing boundary/unknown provider or unstable order: %+v", stats)
	}
	got := stats[1]
	if got.PID != 42 || got.ConnectionCount != 2 || got.Providers["openai"] != 1 || got.Providers["anthropic"] != 1 ||
		got.LastConnection != "2026-01-17T10:06:00Z" || got.ProcessName != "node" {
		t.Fatalf("wrong aggregation or close/alert counted as connection: %+v", got)
	}
	encoded, _ := json.Marshal(stats)
	for _, unsupported := range []string{"request_count", "bytes_in", "bytes_out", "last_request"} {
		if strings.Contains(string(encoded), unsupported) {
			t.Fatalf("connection observations fabricated %s: %s", unsupported, encoded)
		}
	}
	// Fresh calls must not share reducer maps or accumulate the same window.
	got.Providers["openai"] = 999
	again, err := aggregateRanoExport(data, since, until)
	if err != nil || again[1].Providers["openai"] != 1 {
		t.Fatalf("queries share state: %+v %v", again, err)
	}
}

func TestRanoExportPreciseTimeWindow(t *testing.T) {
	since := time.Date(2026, 1, 17, 10, 0, 0, 500000000, time.UTC)
	data := []byte(`{"ts":"2026-01-17T10:00:00.499999999Z","event":"connect","pid":1}
{"ts":"2026-01-17T05:00:00.5-05:00","event":"connect","pid":1}
{"ts":"2026-01-17T10:00:01.499999999Z","event":"connect","pid":1}
{"ts":"2026-01-17T10:00:01.5Z","event":"connect","pid":1}`)
	stats, err := aggregateRanoExport(data, since, since.Add(time.Second))
	if err != nil || len(stats) != 1 || stats[0].ConnectionCount != 2 || stats[0].LastConnection != "2026-01-17T10:00:01.499999999Z" {
		t.Fatalf("local [since, until) filter = %+v %v", stats, err)
	}
}

func TestRanoExportRejectsIncompleteEvidence(t *testing.T) {
	valid := `{"ts":"2026-01-17T10:00:00Z","event":"connect","pid":1}` + "\n"
	cases := []struct{ name, row string }{
		{"null", "null"}, {"array", "[]"}, {"empty-object", "{}"},
		{"unknown-kind", `{"ts":"2026-01-17T10:00:00Z","event":"requests"}`},
		{"missing-time", `{"event":"connect","pid":1}`},
		{"zero-pid", `{"ts":"2026-01-17T10:00:00Z","event":"connect","pid":0}`},
		{"negative-pid", `{"ts":"2026-01-17T10:00:00Z","event":"connect","pid":-1}`},
		{"string-pid", `{"ts":"2026-01-17T10:00:00Z","event":"connect","pid":"1"}`},
		{"fraction-pid", `{"ts":"2026-01-17T10:00:00Z","event":"connect","pid":1.5}`},
		{"truncated", `{"event":`}, {"plain-text", "permission denied"},
		{"oversize-line", fmt.Sprintf(`{"comm":%q}`, strings.Repeat("x", 70*1024))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats, err := aggregateRanoExport([]byte(valid+tc.row), time.Time{}, time.Now().AddDate(100, 0, 0))
			if err == nil || stats != nil {
				t.Fatalf("bad second record became partial success: %+v %v", stats, err)
			}
		})
	}
	for _, data := range []string{"", "\n \n\r\n"} {
		stats, err := aggregateRanoExport([]byte(data), time.Time{}, time.Now())
		if err != nil || stats == nil || len(stats) != 0 {
			t.Fatalf("checked-empty stream = %+v %v", stats, err)
		}
	}
}

func TestRanoWindowDurationContract(t *testing.T) {
	for text, want := range map[string]time.Duration{"": 5 * time.Minute, "5m": 5 * time.Minute, "1h30m": 90 * time.Minute, "1d": 24 * time.Hour, "1w": 7 * 24 * time.Hour, "0.5s": 500 * time.Millisecond} {
		t.Run(text, func(t *testing.T) {
			got, err := RanoWindowDuration(text)
			if err != nil || got != want {
				t.Fatalf("window %q = %v %v", text, got, err)
			}
		})
	}
	for _, text := range []string{"0s", "-1m", "0w", "-2d", "5x", "1.5d", "9223372036854775807h", "18446744073709551615d", "18446744073709551615w"} {
		t.Run(text, func(t *testing.T) {
			if _, err := RanoWindowDuration(text); err == nil {
				t.Fatalf("accepted invalid window %q", text)
			}
		})
	}
}
