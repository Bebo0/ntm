package agentmail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestConfigOptionsEnvironmentOverridesConfig pins the precedence rule every
// Agent Mail call site now shares: NewClient reads AGENT_MAIL_URL /
// AGENT_MAIL_TOKEN before applying options, so a configured value is yielded
// only when the matching variable is unset. Copies of this rule had drifted
// across call sites before ntm#316 consolidated them here.
func TestConfigOptionsEnvironmentOverridesConfig(t *testing.T) {
	const (
		cfgURL   = "http://config.test:9000/mcp/"
		cfgToken = "config-token"
		envURL   = "http://env.test:9100/mcp/"
		envToken = "env-token"
	)

	cases := []struct {
		name      string
		envURL    string
		envToken  string
		wantURL   string
		wantToken string
	}{
		{
			name:      "no environment: config wins",
			wantURL:   cfgURL,
			wantToken: cfgToken,
		},
		{
			name:      "AGENT_MAIL_URL set: environment URL wins, config token still applies",
			envURL:    envURL,
			wantURL:   envURL,
			wantToken: cfgToken,
		},
		{
			name:      "AGENT_MAIL_TOKEN set: environment token wins, config URL still applies",
			envToken:  envToken,
			wantURL:   cfgURL,
			wantToken: envToken,
		},
		{
			name:      "both set: environment wins outright",
			envURL:    envURL,
			envToken:  envToken,
			wantURL:   envURL,
			wantToken: envToken,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_MAIL_URL", tc.envURL)
			t.Setenv("AGENT_MAIL_TOKEN", tc.envToken)

			c := NewClient(ConfigOptions(cfgURL, cfgToken)...)

			if got := strings.TrimSuffix(c.BaseURL(), "/"); got != strings.TrimSuffix(tc.wantURL, "/") {
				t.Errorf("BaseURL() = %q, want %q", got, tc.wantURL)
			}
			if c.bearerToken != tc.wantToken {
				t.Errorf("bearer token = %q, want %q", c.bearerToken, tc.wantToken)
			}
		})
	}
}

// TestConfigOptionsEmptyConfigKeepsDefaults verifies that an unconfigured
// endpoint yields no options at all, leaving the client on its default base
// URL with no bearer.
func TestConfigOptionsEmptyConfigKeepsDefaults(t *testing.T) {
	t.Setenv("AGENT_MAIL_URL", "")
	t.Setenv("AGENT_MAIL_TOKEN", "")
	withoutAmConfig(t)

	if opts := ConfigOptions("", ""); len(opts) != 0 {
		t.Fatalf("ConfigOptions(\"\", \"\") returned %d options, want 0", len(opts))
	}

	c := NewClient(ConfigOptions("", "")...)
	if c.BaseURL() != DefaultBaseURL {
		t.Errorf("BaseURL() = %q, want the default %q", c.BaseURL(), DefaultBaseURL)
	}
	if c.bearerToken != "" {
		t.Errorf("bearer token = %q, want empty", c.bearerToken)
	}
}

// TestUseConfiguredEndpointReachesOptionlessClients pins the precedence a
// client built with no options sees: the recorded configured endpoint, then
// the environment, then explicit options.
func TestUseConfiguredEndpointReachesOptionlessClients(t *testing.T) {
	t.Setenv("AGENT_MAIL_URL", "")
	t.Setenv("AGENT_MAIL_TOKEN", "")
	withoutAmConfig(t)
	t.Cleanup(func() { UseConfiguredEndpoint("", "") })

	UseConfiguredEndpoint("http://config.test:9000/mcp", "config-token")
	c := NewClient()
	if c.BaseURL() != "http://config.test:9000/mcp/" {
		t.Errorf("BaseURL() = %q, want the configured endpoint with a trailing slash", c.BaseURL())
	}
	if c.bearerToken != "config-token" {
		t.Errorf("bearer token = %q, want the configured token", c.bearerToken)
	}

	t.Setenv("AGENT_MAIL_URL", "http://env.test:9100/mcp/")
	t.Setenv("AGENT_MAIL_TOKEN", "env-token")
	c = NewClient()
	if c.BaseURL() != "http://env.test:9100/mcp/" || c.bearerToken != "env-token" {
		t.Errorf("environment must override the configured endpoint; got %q / %q", c.BaseURL(), c.bearerToken)
	}

	c = NewClient(WithBaseURL("http://option.test/mcp/"), WithToken("option-token"))
	if c.BaseURL() != "http://option.test/mcp/" || c.bearerToken != "option-token" {
		t.Errorf("explicit options must override both; got %q / %q", c.BaseURL(), c.bearerToken)
	}

	t.Setenv("AGENT_MAIL_URL", "")
	t.Setenv("AGENT_MAIL_TOKEN", "")
	UseConfiguredEndpoint("", "")
	c = NewClient()
	if c.BaseURL() != DefaultBaseURL || c.bearerToken != "" {
		t.Errorf("cleared endpoint must fall back to the defaults; got %q / %q", c.BaseURL(), c.bearerToken)
	}
}

// TestQuickAvailableDecidesWithoutRetrying pins the contract that separates
// the inventory probe from the dispatch gate: one request, a clear verdict,
// and "cannot decide" reserved for a liveness endpoint that is missing or
// auth-walled. IsAvailableContext retries with backoff by design, which is
// right for gating a send and ~1.25s too slow for filling a tools row
// (ntm#316).
func TestQuickAvailableDecidesWithoutRetrying(t *testing.T) {
	t.Setenv("AGENT_MAIL_URL", "")
	t.Setenv("AGENT_MAIL_TOKEN", "")
	withoutAmConfig(t)

	newServer := func(t *testing.T, status int, requireToken string) (*httptest.Server, *atomic.Int64) {
		t.Helper()
		var requests atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if r.URL.Path != "/"+HealthCheckPath {
				http.NotFound(w, r)
				return
			}
			if requireToken != "" && r.Header.Get("Authorization") != "Bearer "+requireToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		return srv, &requests
	}

	t.Run("2xx is available and decided in one request", func(t *testing.T) {
		srv, requests := newServer(t, http.StatusOK, "")
		c := NewClient(WithBaseURL(srv.URL))

		available, decided := c.QuickAvailable(context.Background())
		if !available || !decided {
			t.Errorf("available=%v decided=%v, want true/true", available, decided)
		}
		if got := requests.Load(); got != 1 {
			t.Errorf("made %d requests, want exactly 1 (no retries)", got)
		}
	})

	t.Run("5xx is a decided failure", func(t *testing.T) {
		srv, _ := newServer(t, http.StatusServiceUnavailable, "")
		c := NewClient(WithBaseURL(srv.URL))

		available, decided := c.QuickAvailable(context.Background())
		if available || !decided {
			t.Errorf("available=%v decided=%v, want false/true", available, decided)
		}
	})

	t.Run("auth-walled liveness cannot decide, so the caller may escalate", func(t *testing.T) {
		srv, _ := newServer(t, http.StatusOK, "needed-token")
		c := NewClient(WithBaseURL(srv.URL)) // no token

		available, decided := c.QuickAvailable(context.Background())
		if decided {
			t.Errorf("a 401 decided the question (available=%v); it must defer to the MCP probe", available)
		}
	})

	t.Run("a bearer-accepting server is available", func(t *testing.T) {
		srv, _ := newServer(t, http.StatusOK, "needed-token")
		c := NewClient(WithBaseURL(srv.URL), WithToken("needed-token"))

		if available, decided := c.QuickAvailable(context.Background()); !available || !decided {
			t.Errorf("available=%v decided=%v, want true/true with the right bearer", available, decided)
		}
	})

	t.Run("a dead port is decided immediately", func(t *testing.T) {
		c := NewClient(WithBaseURL("http://127.0.0.1:1"))

		start := time.Now()
		available, decided := c.QuickAvailable(context.Background())
		elapsed := time.Since(start)

		if available || !decided {
			t.Errorf("available=%v decided=%v, want false/true", available, decided)
		}
		if elapsed > 300*time.Millisecond {
			t.Errorf("a refused connection took %s; QuickAvailable must not retry", elapsed)
		}
	})
}

// withoutAmConfig points XDG_CONFIG_HOME at an empty directory so a test that
// expects no bearer never picks up the developer's own am token.
func withoutAmConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// withAmConfig writes contents as am's config.env under a fresh
// XDG_CONFIG_HOME, the file `am setup` stores the server's token in.
func withAmConfig(t *testing.T, contents string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "mcp-agent-mail"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp-agent-mail", "config.env"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestNewClientUsesAmTokenForLocalServer: with no token configured, a client
// for a loopback server presents the token am set its server up with, and a
// server that demands it accepts the client. A configured token still wins,
// and a non-loopback server never receives am's token.
func TestNewClientUsesAmTokenForLocalServer(t *testing.T) {
	t.Setenv("AGENT_MAIL_URL", "")
	t.Setenv("AGENT_MAIL_TOKEN", "")
	withAmConfig(t, "# written by am setup\nHTTP_BEARER_TOKEN=am-token\n")

	if c := NewClient(); c.bearerToken != "am-token" {
		t.Errorf("default endpoint: bearer = %q, want am's token", c.bearerToken)
	}
	for _, base := range []string{"http://localhost:8765/mcp/", "http://[::1]:8765/mcp/", "http://127.0.0.2:9/api/"} {
		if c := NewClient(WithBaseURL(base)); c.bearerToken != "am-token" {
			t.Errorf("%s: bearer = %q, want am's token", base, c.bearerToken)
		}
	}

	var unauthorized atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer am-token" {
			unauthorized.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	if available, decided := NewClient(WithBaseURL(srv.URL)).QuickAvailable(context.Background()); !available || !decided || unauthorized.Load() != 0 {
		t.Errorf("token-walled local server: available=%v decided=%v rejected=%d, want accepted", available, decided, unauthorized.Load())
	}

	for _, base := range []string{"http://mail.example.test/mcp/", "http://10.0.0.5:8765/mcp/", "http://127.0.0.1.example.test/mcp/"} {
		if c := NewClient(WithBaseURL(base)); c.bearerToken != "" {
			t.Errorf("%s: bearer = %q, am's token must not leave the machine", base, c.bearerToken)
		}
	}
	t.Setenv("AGENT_MAIL_URL", "http://mail.example.test/mcp/")
	if c := NewClient(); c.bearerToken != "" {
		t.Errorf("AGENT_MAIL_URL remote: bearer = %q, am's token must not leave the machine", c.bearerToken)
	}
	t.Setenv("AGENT_MAIL_URL", "")

	if c := NewClient(ConfigOptions("", "config-token")...); c.bearerToken != "config-token" {
		t.Errorf("configured token: bearer = %q, want it over am's", c.bearerToken)
	}
	t.Setenv("AGENT_MAIL_TOKEN", "env-token")
	if c := NewClient(); c.bearerToken != "env-token" {
		t.Errorf("AGENT_MAIL_TOKEN: bearer = %q, want it over am's", c.bearerToken)
	}
}

// TestAmConfigTokenLocation: am keeps config.env under XDG_CONFIG_HOME only
// when that is absolute, otherwise under ~/.config; a missing file is no token.
func TestAmConfigTokenLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	if got := amConfigToken(); got != "" {
		t.Fatalf("no config.env: token = %q, want empty", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "mcp-agent-mail"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "mcp-agent-mail", "config.env"), []byte("HTTP_BEARER_TOKEN=home-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := amConfigToken(); got != "home-token" {
		t.Errorf("relative XDG_CONFIG_HOME: token = %q, want the ~/.config one", got)
	}
	withAmConfig(t, "HTTP_BEARER_TOKEN=xdg-token\n")
	if got := amConfigToken(); got != "xdg-token" {
		t.Errorf("absolute XDG_CONFIG_HOME: token = %q, want the XDG one", got)
	}
}

func TestDotenvValue(t *testing.T) {
	cases := []struct {
		name, contents, want string
	}{
		{"plain", "HTTP_BEARER_TOKEN=abc123", "abc123"},
		{"missing", "HTTP_PORT=8765\n", ""},
		{"longer key is not the key", "HTTP_BEARER_TOKEN_OLD=stale\n", ""},
		{"comment and blank lines", "# HTTP_BEARER_TOKEN=commented\n\nHTTP_BEARER_TOKEN=live\n", "live"},
		{"export prefix", "export HTTP_BEARER_TOKEN=exported", "exported"},
		{"spaces around", "  HTTP_BEARER_TOKEN = spaced  ", "spaced"},
		{"double quotes", `HTTP_BEARER_TOKEN="quoted value"`, "quoted value"},
		{"single quotes", `HTTP_BEARER_TOKEN='single'`, "single"},
		{"inline comment", "HTTP_BEARER_TOKEN=tok # rotated monday", "tok"},
		{"tab before comment", "HTTP_BEARER_TOKEN=tok\t# note", "tok"},
		{"hash inside value", "HTTP_BEARER_TOKEN=a#b", "a#b"},
		{"hash inside quotes", `HTTP_BEARER_TOKEN="a #b" # note`, "a #b"},
		{"equals inside value", "HTTP_BEARER_TOKEN=base64==", "base64=="},
		{"last assignment wins", "HTTP_BEARER_TOKEN=old\nHTTP_BEARER_TOKEN=new\n", "new"},
		{"CRLF line endings", "HTTP_BEARER_TOKEN=crlf\r\nHTTP_PORT=1\r\n", "crlf"},
		{"empty value", "HTTP_BEARER_TOKEN=", ""},
	}
	for _, tc := range cases {
		if got := dotenvValue(tc.contents, "HTTP_BEARER_TOKEN"); got != tc.want {
			t.Errorf("%s: dotenvValue = %q, want %q", tc.name, got, tc.want)
		}
	}
}
