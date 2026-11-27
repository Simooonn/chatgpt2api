package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type upgradeRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn upgradeRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func setupUpgradeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "outbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeUpgradeHeartbeat(t *testing.T, dir string, updatedAt time.Time, currentTag string) {
	t.Helper()
	data, err := json.Marshal(agentHeartbeatPayload{
		Version:    1,
		ImageRepo:  "ghcr.io/simooonn/chatgpt2api",
		CurrentTag: currentTag,
		UpdatedAt:  updatedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outbox", "agent.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeListReleasesFiltersForkTags(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: upgradeRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/repos/Simooonn/chatgpt2api/releases" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		body := `[
			{"tag_name":"v2.1.0-roamai.2","name":"v2.1.0-roamai.2","html_url":"https://example.test/2","body":"Notes","draft":false,"published_at":"2026-11-27T01:00:00Z"},
			{"tag_name":"v2.1.0-roamai.3","name":"draft","html_url":"https://example.test/3","body":"Draft","draft":true,"published_at":"2026-11-27T02:00:00Z"},
			{"tag_name":"v2.1.0","name":"upstream","html_url":"https://example.test/u","body":"Up","draft":false,"published_at":"2026-11-22T00:00:00Z"},
			{"tag_name":"v2.1.0-roamai.1","name":"","html_url":"https://example.test/1","body":"Init","draft":false,"published_at":"2026-11-26T00:00:00Z"}
		]`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	svc := NewUpgradeService(UpgradeConfig{CurrentVersion: "2.1.0-roamai.1"}, client)
	list, err := svc.ListReleases(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Tag != "v2.1.0-roamai.2" || list[0].Version != "2.1.0-roamai.2" || list[1].Tag != "v2.1.0-roamai.1" {
		t.Fatalf("list = %#v", list)
	}
	if _, err := svc.ListReleases(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 cached call, got %d", calls)
	}
	if _, err := svc.ListReleases(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls after force, got %d", calls)
	}
}

func TestUpgradeRequestLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	if _, err := NewUpgradeService(UpgradeConfig{}, nil).RequestUpgrade(context.Background(), "v2.1.0-roamai.1", "admin"); !errors.Is(err, ErrUpgradeNotConfigured) {
		t.Fatalf("err = %v, want ErrUpgradeNotConfigured", err)
	}

	dir := setupUpgradeDir(t)
	svc := NewUpgradeService(UpgradeConfig{ControlDir: dir, CurrentVersion: "2.1.0-roamai.1"}, nil)
	svc.now = func() time.Time { return now }

	for _, bad := range []string{"latest", "v2.1.0", "v2.1.0-roamai.0", "v2.1.0-roamai.1;id"} {
		if _, err := svc.RequestUpgrade(context.Background(), bad, "admin"); !errors.Is(err, ErrUpgradeInvalidTag) {
			t.Fatalf("tag %q: err = %v", bad, err)
		}
	}

	if _, err := svc.RequestUpgrade(context.Background(), "v2.1.0-roamai.2", "admin"); !errors.Is(err, ErrUpgradeAgentOffline) {
		t.Fatalf("err = %v, want ErrUpgradeAgentOffline", err)
	}

	writeUpgradeHeartbeat(t, dir, now.Add(-20*time.Second), "v2.1.0-roamai.1")
	op, err := svc.RequestUpgrade(context.Background(), "v2.1.0-roamai.2", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if op.Phase != "queued" || op.Tag != "v2.1.0-roamai.2" || len(op.ID) != 16 {
		t.Fatalf("op = %#v", op)
	}

	if st := svc.Status(); !st.AgentOnline || !st.InProgress || st.Operation == nil || st.Operation.Phase != "queued" {
		t.Fatalf("status = %#v", st)
	}
	if _, err := svc.RequestUpgrade(context.Background(), "v2.1.0-roamai.2", "admin"); !errors.Is(err, ErrUpgradeInProgress) {
		t.Fatalf("err = %v, want ErrUpgradeInProgress", err)
	}
}
