package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	DefaultReleaseRepo      = "Simooonn/chatgpt2api"
	upgradeMaxResponseSize  = 2 << 20
	upgradeMaxControlSize   = 64 << 10
	upgradeMaxNotesRunes    = 2048
	upgradeReleasesCacheTTL = 60 * time.Second
	upgradeHeartbeatMaxAge  = 5 * time.Minute
	upgradeActiveMaxStale   = 15 * time.Minute
)

// ForkTagPattern matches fork release tags like v2.1.0-roamai.1.
// Must stay identical to TAG_PATTERN in deploy/upgrade-agent/chatgpt2api-upgrade-agent.py.
var ForkTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?-roamai\.[1-9][0-9]*$`)

var (
	ErrUpgradeNotConfigured = errors.New("未配置升级控制目录，无法在线升级")
	ErrUpgradeInvalidTag    = errors.New("版本号格式无效，仅支持 vX.Y.Z-roamai.N")
	ErrUpgradeAgentOffline  = errors.New("宿主机升级代理未在线，请先检查 chatgpt2api-upgrade-agent 服务")
	ErrUpgradeInProgress    = errors.New("当前已有升级任务正在执行，请等待完成")
)

type UpgradeConfig struct {
	ControlDir     string
	ReleaseRepo    string
	CurrentVersion string
}

type ForkRelease struct {
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Notes       string `json:"notes"`
	PublishedAt string `json:"published_at"`
}

type UpgradeOperation struct {
	ID          string `json:"id"`
	Tag         string `json:"tag"`
	Phase       string `json:"phase"`
	Message     string `json:"message"`
	FromVersion string `json:"from_version"`
	RequestedBy string `json:"requested_by"`
	UpdatedAt   string `json:"updated_at"`
}

type UpgradeStatus struct {
	Enabled        bool              `json:"enabled"`
	AgentOnline    bool              `json:"agent_online"`
	AgentUpdatedAt string            `json:"agent_updated_at"`
	ImageRepo      string            `json:"image_repo"`
	CurrentTag     string            `json:"current_tag"`
	CurrentVersion string            `json:"current_version"`
	ReleaseRepo    string            `json:"release_repo"`
	InProgress     bool              `json:"in_progress"`
	Operation      *UpgradeOperation `json:"operation"`
}

type agentHeartbeatPayload struct {
	Version    int    `json:"version"`
	ImageRepo  string `json:"image_repo"`
	CurrentTag string `json:"current_tag"`
	UpdatedAt  string `json:"updated_at"`
}

type upgradeRequestPayload struct {
	ID          string `json:"id"`
	Tag         string `json:"tag"`
	FromVersion string `json:"from_version"`
	RequestedBy string `json:"requested_by"`
	RequestedAt string `json:"requested_at"`
}

type UpgradeService struct {
	controlDir     string
	releaseRepo    string
	currentVersion string
	client         *http.Client
	now            func() time.Time

	mu              sync.Mutex
	cachedReleases  []ForkRelease
	releasesFetched time.Time
}

func NewUpgradeService(cfg UpgradeConfig, client *http.Client) *UpgradeService {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	repo := strings.Trim(strings.TrimSpace(cfg.ReleaseRepo), "/")
	if strings.Count(repo, "/") != 1 {
		repo = DefaultReleaseRepo
	}
	current := strings.TrimPrefix(strings.TrimSpace(cfg.CurrentVersion), "v")
	if current == "" {
		current = "0.0.0"
	}
	return &UpgradeService{
		controlDir:     strings.TrimSpace(cfg.ControlDir),
		releaseRepo:    repo,
		currentVersion: current,
		client:         client,
		now:            time.Now,
	}
}

func (s *UpgradeService) Enabled() bool { return s.controlDir != "" }

func (s *UpgradeService) inboxDir() string  { return filepath.Join(s.controlDir, "inbox") }
func (s *UpgradeService) outboxDir() string { return filepath.Join(s.controlDir, "outbox") }

func (s *UpgradeService) ListReleases(ctx context.Context, force bool) ([]ForkRelease, error) {
	s.mu.Lock()
	if !force && !s.releasesFetched.IsZero() && s.now().Sub(s.releasesFetched) < upgradeReleasesCacheTTL {
		copied := append([]ForkRelease(nil), s.cachedReleases...)
		s.mu.Unlock()
		return copied, nil
	}
	s.mu.Unlock()

	apiURL := "https://api.github.com/repos/" + s.releaseRepo + "/releases?per_page=20"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "chatgpt2api/"+s.currentVersion)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询发布版本列表失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询发布版本列表失败（HTTP %d）", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, upgradeMaxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("读取发布版本列表响应: %w", err)
	}
	if len(data) > upgradeMaxResponseSize {
		return nil, errors.New("发布版本列表响应超过安全上限")
	}
	var payload []struct {
		Tag         string `json:"tag_name"`
		Name        string `json:"name"`
		HTMLURL     string `json:"html_url"`
		Body        string `json:"body"`
		Draft       bool   `json:"draft"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("解析发布版本列表响应: %w", err)
	}
	releases := make([]ForkRelease, 0, len(payload))
	for _, item := range payload {
		tag := strings.TrimSpace(item.Tag)
		if item.Draft || !ForkTagPattern.MatchString(tag) {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = tag
		}
		releases = append(releases, ForkRelease{
			Tag:         tag,
			Version:     strings.TrimPrefix(tag, "v"),
			Name:        name,
			URL:         strings.TrimSpace(item.HTMLURL),
			Notes:       truncateUpgradeRunes(strings.TrimSpace(item.Body), upgradeMaxNotesRunes),
			PublishedAt: strings.TrimSpace(item.PublishedAt),
		})
	}
	s.mu.Lock()
	s.cachedReleases = append([]ForkRelease(nil), releases...)
	s.releasesFetched = s.now()
	s.mu.Unlock()
	return releases, nil
}

func (s *UpgradeService) Status() UpgradeStatus {
	result := UpgradeStatus{
		Enabled:        s.Enabled(),
		CurrentVersion: s.currentVersion,
		ReleaseRepo:    s.releaseRepo,
	}
	if !result.Enabled {
		return result
	}

	now := s.now().UTC()
	if raw, err := readUpgradeControlFile(filepath.Join(s.outboxDir(), "agent.json")); err == nil {
		var hb agentHeartbeatPayload
		if json.Unmarshal(raw, &hb) == nil {
			result.ImageRepo = strings.TrimSpace(hb.ImageRepo)
			result.CurrentTag = strings.TrimSpace(hb.CurrentTag)
			result.AgentUpdatedAt = strings.TrimSpace(hb.UpdatedAt)
			if parsed, err := time.Parse(time.RFC3339, result.AgentUpdatedAt); err == nil {
				age := now.Sub(parsed)
				if age >= -time.Minute && age <= upgradeHeartbeatMaxAge {
					result.AgentOnline = true
				}
			}
		}
	}

	if raw, err := readUpgradeControlFile(filepath.Join(s.inboxDir(), "request.json")); err == nil {
		var req upgradeRequestPayload
		if json.Unmarshal(raw, &req) == nil && strings.TrimSpace(req.Tag) != "" {
			result.Operation = &UpgradeOperation{
				ID:          req.ID,
				Tag:         req.Tag,
				Phase:       "queued",
				Message:     "升级请求已提交，等待宿主机代理开始执行",
				FromVersion: req.FromVersion,
				RequestedBy: req.RequestedBy,
				UpdatedAt:   req.RequestedAt,
			}
			result.InProgress = true
			return result
		}
	}

	if raw, err := readUpgradeControlFile(filepath.Join(s.outboxDir(), "status.json")); err == nil {
		var op UpgradeOperation
		if json.Unmarshal(raw, &op) == nil && strings.TrimSpace(op.Phase) != "" {
			op.ID = strings.TrimSpace(op.ID)
			op.Tag = strings.TrimSpace(op.Tag)
			op.Phase = strings.TrimSpace(op.Phase)
			op.Message = strings.TrimSpace(op.Message)
			op.FromVersion = strings.TrimSpace(op.FromVersion)
			op.RequestedBy = strings.TrimSpace(op.RequestedBy)
			op.UpdatedAt = strings.TrimSpace(op.UpdatedAt)
			result.Operation = &op
			result.InProgress = isUpgradePhaseActive(op.Phase) && !isUpgradeOpStale(op.UpdatedAt, now)
		}
	}

	return result
}

func (s *UpgradeService) RequestUpgrade(_ context.Context, tag, requestedBy string) (UpgradeOperation, error) {
	if !s.Enabled() {
		return UpgradeOperation{}, ErrUpgradeNotConfigured
	}
	tag = strings.TrimSpace(tag)
	if !ForkTagPattern.MatchString(tag) {
		return UpgradeOperation{}, ErrUpgradeInvalidTag
	}
	requestedBy = sanitizeUpgradeField(requestedBy, 64)
	if requestedBy == "" {
		requestedBy = "admin"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	status := s.Status()
	if !status.AgentOnline {
		return UpgradeOperation{}, ErrUpgradeAgentOffline
	}
	if status.InProgress {
		return UpgradeOperation{}, ErrUpgradeInProgress
	}

	id, err := randomUpgradeHex(8)
	if err != nil {
		return UpgradeOperation{}, fmt.Errorf("生成请求 ID 失败: %w", err)
	}
	nowStr := s.now().UTC().Format(time.RFC3339)
	req := upgradeRequestPayload{
		ID:          id,
		Tag:         tag,
		FromVersion: "v" + s.currentVersion,
		RequestedBy: requestedBy,
		RequestedAt: nowStr,
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return UpgradeOperation{}, err
	}

	inbox := s.inboxDir()
	tmpFile, err := os.CreateTemp(inbox, ".req-*.tmp")
	if err != nil {
		return UpgradeOperation{}, fmt.Errorf("无法写入升级请求目录: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmpFile.Chmod(0o644); err != nil {
		_ = tmpFile.Close()
		return UpgradeOperation{}, err
	}
	if _, err := tmpFile.Write(encoded); err != nil {
		_ = tmpFile.Close()
		return UpgradeOperation{}, err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return UpgradeOperation{}, err
	}
	if err := tmpFile.Close(); err != nil {
		return UpgradeOperation{}, err
	}
	if err := os.Rename(tmpPath, filepath.Join(inbox, "request.json")); err != nil {
		return UpgradeOperation{}, fmt.Errorf("提交升级请求失败: %w", err)
	}
	cleanup = false

	return UpgradeOperation{
		ID:          id,
		Tag:         tag,
		Phase:       "queued",
		Message:     "升级请求已提交，等待宿主机代理开始执行",
		FromVersion: req.FromVersion,
		RequestedBy: requestedBy,
		UpdatedAt:   nowStr,
	}, nil
}

func isUpgradePhaseActive(phase string) bool {
	switch phase {
	case "queued", "backing_up", "pulling", "recreating", "health_check", "rolling_back":
		return true
	default:
		return false
	}
}

func isUpgradeOpStale(updatedAt string, now time.Time) bool {
	parsed, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return true
	}
	return now.Sub(parsed) > upgradeActiveMaxStale
}

func readUpgradeControlFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, upgradeMaxControlSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > upgradeMaxControlSize {
		return nil, errors.New("control file too large")
	}
	return data, nil
}

func randomUpgradeHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func sanitizeUpgradeField(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if r >= 0x20 && r != 0x7f && r != '\t' && r != '\n' && r != '\r' {
			b.WriteRune(r)
		}
	}
	return truncateUpgradeRunes(b.String(), maxLen)
}

func truncateUpgradeRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
