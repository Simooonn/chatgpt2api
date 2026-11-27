package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

type SystemHandler struct {
	config  *config.Config
	proxy   *service.ProxyService
	upgrade *service.UpgradeService
}

func NewSystemHandler(config *config.Config, proxy *service.ProxyService) *SystemHandler {
	return &SystemHandler{
		config:  config,
		proxy:   proxy,
		upgrade: service.NewUpgradeService(service.UpgradeConfig{CurrentVersion: config.Version}, nil),
	}
}

func (h *SystemHandler) WithUpgrade(upgrade *service.UpgradeService) *SystemHandler {
	if upgrade != nil {
		h.upgrade = upgrade
	}
	return h
}

func (h *SystemHandler) RegisterVersion(router *gin.Engine) { router.GET("/version", h.version) }

func (h *SystemHandler) RegisterRoutes(router *gin.RouterGroup) {
	router.GET("/settings", h.settings)
	router.POST("/settings", h.updateSettings)
	router.POST("/proxy/test", h.testProxy)
	router.POST("/proxy/clearance/test", h.testClearance)
	router.GET("/system/releases", h.releases)
	router.GET("/system/upgrade/status", h.upgradeStatus)
	router.POST("/system/upgrade", h.requestUpgrade)
}

func (h *SystemHandler) testProxy(c *gin.Context) {
	var body ProxyTestRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"result": h.proxy.Test(c.Request.Context(), body.URL)})
}
func (h *SystemHandler) testClearance(c *gin.Context) {
	var body ClearanceTestRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, err)
		return
	}
	bundle, err := h.proxy.RefreshClearance(c.Request.Context(), body.TargetURL)
	if err != nil {
		writeResult(c, nil, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"result": gin.H{"ok": true, "cookie_count": len(bundle.Cookies), "expires_at": bundle.ExpiresAt}})
}

func (h *SystemHandler) version(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"version": h.config.Version})
}

func (h *SystemHandler) settings(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"config": h.config.Public()})
}

func (h *SystemHandler) updateSettings(c *gin.Context) {
	var body map[string]any
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, errors.New("设置内容格式不正确"))
		return
	}
	result, err := h.config.Update(body)
	writeResult(c, gin.H{"config": result}, err)
}

func (h *SystemHandler) releases(c *gin.Context) {
	force := c.Query("force") == "1" || strings.EqualFold(c.Query("force"), "true")
	list, err := h.upgrade.ListReleases(c.Request.Context(), force)
	if err != nil {
		writeError(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"releases": list})
}

func (h *SystemHandler) upgradeStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.upgrade.Status())
}

func (h *SystemHandler) requestUpgrade(c *gin.Context) {
	var body UpgradeRequest
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Tag) == "" {
		writeError(c, http.StatusBadRequest, errors.New("缺少要升级的版本号"))
		return
	}
	requestedBy := "admin"
	if value, ok := c.Get("identity"); ok {
		if id, ok := value.(service.Identity); ok && strings.TrimSpace(id.Name) != "" {
			requestedBy = id.Name
		}
	}
	op, err := h.upgrade.RequestUpgrade(c.Request.Context(), strings.TrimSpace(body.Tag), requestedBy)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrUpgradeInvalidTag):
			writeError(c, http.StatusBadRequest, err)
		case errors.Is(err, service.ErrUpgradeNotConfigured), errors.Is(err, service.ErrUpgradeAgentOffline):
			writeError(c, http.StatusServiceUnavailable, err)
		case errors.Is(err, service.ErrUpgradeInProgress):
			writeError(c, http.StatusConflict, err)
		default:
			writeError(c, http.StatusInternalServerError, err)
		}
		return
	}
	c.JSON(http.StatusAccepted, op)
}

type ProxyTestRequest struct {
	URL string `json:"url"`
}
type ClearanceTestRequest struct {
	TargetURL string `json:"target_url" binding:"required"`
}
type UpgradeRequest struct {
	Tag string `json:"tag"`
}
