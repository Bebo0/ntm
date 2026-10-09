package tmux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var ompPaneID = regexp.MustCompile(`^%[0-9]+$`)

// ErrOMPDeliveryUncertain means a mutating request may have reached OMP.
// Inspect its request ID before retrying; never fall back to terminal input.
var ErrOMPDeliveryUncertain = errors.New("OMP delivery outcome uncertain; inspect the existing request before retrying")

// OMPControlContext controls the native OMP process; it never emits terminal
// input. The worker installs private socket/instance metadata on its own pane.
func OMPControlContext(ctx context.Context, paneID, path string, payload any) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !ompPaneID.MatchString(paneID) {
		return nil, errors.New("OMP requires an explicit pane ID")
	}
	if DefaultClient.Remote != "" {
		return nil, errors.New("OMP native control must run on the worker that owns the Unix socket")
	}
	switch path {
	case "/state":
		if payload != nil {
			return nil, errors.New("OMP state is read-only")
		}
	case "/send", "/stage", "/interrupt", "/shutdown":
		if payload == nil {
			return nil, errors.New("OMP mutation requires a payload")
		}
	default:
		return nil, errors.New("unsupported OMP control operation")
	}
	metadata, err := DefaultClient.RunContext(ctx, "display-message", "-p", "-t", paneID, "#{@omp_socket}|#{@omp_instance}|#{@omp_run}")
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimSpace(metadata), "|")
	if len(parts) != 3 || !filepath.IsAbs(parts[0]) || parts[1] == "" || parts[2] == "" {
		return nil, errors.New("OMP native adapter metadata missing")
	}
	socket, instance, run := parts[0], parts[1], parts[2]
	info, err := os.Lstat(socket)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("OMP socket must be private")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Validate the runtime before exposing a prompt or issuing a mutation.
	// The instance header also makes the extension reject a restart race.
	if path != "/state" {
		if _, err := ompControlRequest(ctx, client, instance, run, "/state", nil); err != nil {
			return nil, err
		}
	}
	return ompControlRequest(ctx, client, instance, run, path, payload)
}

func ompControlRequest(ctx context.Context, client *http.Client, instance, run, path string, payload any) ([]byte, error) {
	method := http.MethodGet
	var data []byte
	var err error
	if payload != nil {
		method = http.MethodPost
		data, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://omp"+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-OMP-Instance", instance)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if method == http.MethodPost {
			return nil, fmt.Errorf("%w: %v", ErrOMPDeliveryUncertain, err)
		}
		return nil, fmt.Errorf("OMP control unavailable (no keystroke fallback): %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		if method != http.MethodPost {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrOMPDeliveryUncertain, err)
	}
	if len(body) > 1<<20 {
		if method != http.MethodPost {
			return nil, errors.New("OMP response too large")
		}
		return nil, fmt.Errorf("%w: oversized response", ErrOMPDeliveryUncertain)
	}
	if response.StatusCode != http.StatusOK {
		if method == http.MethodPost && response.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: HTTP %d", ErrOMPDeliveryUncertain, response.StatusCode)
		}
		return nil, fmt.Errorf("OMP rejected operation: HTTP %d: %s", response.StatusCode, body)
	}
	var identity struct {
		Instance string `json:"instance_id"`
		Run      string `json:"run_id"`
		Ready    bool   `json:"ready"`
		Status   string `json:"status"`
	}
	if json.Unmarshal(body, &identity) != nil || identity.Instance != instance || (identity.Run != "" && identity.Run != run) {
		if method != http.MethodPost {
			return nil, errors.New("OMP response instance/run mismatch")
		}
		return nil, fmt.Errorf("%w: response instance mismatch", ErrOMPDeliveryUncertain)
	}
	if path == "/state" && (identity.Run != run || !identity.Ready) {
		return nil, errors.New("OMP runtime is not ready")
	}
	if path == "/send" && identity.Status != "queued" || path == "/stage" && identity.Status != "staged" {
		return nil, fmt.Errorf("%w: receipt status %q", ErrOMPDeliveryUncertain, identity.Status)
	}
	return body, nil
}

func VerifyOMPControlContext(ctx context.Context, paneID string) error {
	_, err := OMPControlContext(ctx, paneID, "/state", nil)
	return err
}
