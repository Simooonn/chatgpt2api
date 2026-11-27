package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

type systemRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn systemRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestSystemHandlerUpgradeRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "outbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	hb := `{"version":1,"image_repo":"ghcr.io/simooonn/chatgpt2api","current_tag":"v2.1.0-roamai.1","updated_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(dir, "outbox", "agent.json"), []byte(hb), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: systemRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `[{"tag_name":"v2.1.0-roamai.2","name":"v2.1.0-roamai.2","html_url":"https://example.test/2","body":"Notes","draft":false,"published_at":"2026-11-27T00:00:00Z"}]`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}

	cfg := &config.Config{Version: "2.1.0-roamai.1"}
	upgrades := service.NewUpgradeService(service.UpgradeConfig{ControlDir: dir, CurrentVersion: cfg.Version}, client)
	router := gin.New()
	h := NewSystemHandler(cfg, nil).WithUpgrade(upgrades)
	h.RegisterVersion(router)
	h.RegisterRoutes(router.Group("/api"))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/system/releases", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "v2.1.0-roamai.2") {
		t.Fatalf("releases status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/system/upgrade/status", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"agent_online":true`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/system/upgrade", strings.NewReader(`{"tag":"latest"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad tag status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/system/upgrade", strings.NewReader(`{"tag":"v2.1.0-roamai.2"}`)))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"phase":"queued"`) {
		t.Fatalf("upgrade status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/system/upgrade", strings.NewReader(`{"tag":"v2.1.0-roamai.2"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", rec.Code, rec.Body.String())
	}
}
