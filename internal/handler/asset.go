package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cyberstrike-ai/internal/database"
	"cyberstrike-ai/internal/security"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type AssetHandler struct {
	db     *database.DB
	logger *zap.Logger
}

func NewAssetHandler(db *database.DB, logger *zap.Logger) *AssetHandler {
	return &AssetHandler{db: db, logger: logger}
}

func assetAccess(c *gin.Context) database.RBACListAccess {
	if session, ok := security.CurrentSession(c); ok {
		return database.RBACListAccess{UserID: session.UserID, Scope: session.Scope}
	}
	return database.RBACListAccess{}
}

type importAssetsRequest struct {
	Assets      []*database.Asset `json:"assets" binding:"required"`
	Source      string            `json:"source"`
	SourceQuery string            `json:"source_query"`
}

type assetScanLink struct {
	AssetID        string `json:"asset_id" binding:"required"`
	ConversationID string `json:"conversation_id"`
	QueueID        string `json:"queue_id"`
	TaskID         string `json:"task_id"`
}

type recordAssetScansRequest struct {
	Scans []assetScanLink `json:"scans" binding:"required"`
}

type updateAssetsProjectRequest struct {
	AssetIDs  []string `json:"asset_ids" binding:"required"`
	ProjectID string   `json:"project_id"`
}

func (h *AssetHandler) Import(c *gin.Context) {
	var req importAssetsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Assets) == 0 || len(req.Assets) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "assets 数量必须在 1-1000 之间"})
		return
	}
	owner := ""
	allowGlobal := false
	if session, ok := security.CurrentSession(c); ok {
		owner = session.UserID
		allowGlobal = session.Scope == database.RBACScopeAll
	}
	for _, asset := range req.Assets {
		if asset == nil {
			continue
		}
		if strings.TrimSpace(asset.ProjectID) != "" {
			if session, ok := security.CurrentSession(c); ok && !h.db.UserCanAccessResource(session.UserID, session.Scope, "project", strings.TrimSpace(asset.ProjectID)) {
				c.JSON(http.StatusForbidden, gin.H{"error": "无权绑定该项目"})
				return
			}
		}
		if strings.TrimSpace(asset.Source) == "" {
			asset.Source = strings.TrimSpace(req.Source)
		}
		if strings.TrimSpace(asset.SourceQuery) == "" {
			asset.SourceQuery = strings.TrimSpace(req.SourceQuery)
		}
	}
	result, err := h.db.UpsertAssets(req.Assets, owner, allowGlobal)
	if err != nil {
		var validationErr *database.AssetValidationError
		if errors.As(err, &validationErr) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("导入资产失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *AssetHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	filter, err := assetListFilterFromQuery(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	assets, total, err := h.db.ListAssets(pageSize, (page-1)*pageSize, filter, assetAccess(c))
	if err != nil {
		h.logger.Error("加载资产失败", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	c.JSON(http.StatusOK, gin.H{"assets": assets, "total": total, "page": page, "page_size": pageSize, "total_pages": totalPages})
}

func assetListFilterFromQuery(c *gin.Context) (database.AssetListFilter, error) {
	filter := database.AssetListFilter{
		Search: strings.TrimSpace(c.Query("q")), Status: strings.ToLower(strings.TrimSpace(c.Query("status"))),
		Protocol: strings.ToLower(strings.TrimSpace(c.Query("protocol"))), ProjectID: strings.TrimSpace(c.Query("project_id")),
		Source: strings.TrimSpace(c.Query("source")), Tag: strings.TrimSpace(c.Query("tag")), Host: strings.TrimSpace(c.Query("host")),
		IP: strings.TrimSpace(c.Query("ip")), Domain: strings.TrimSpace(c.Query("domain")), ScanState: strings.ToLower(strings.TrimSpace(c.Query("scan_state"))),
		SortBy: strings.ToLower(strings.TrimSpace(c.Query("sort_by"))), SortOrder: strings.ToLower(strings.TrimSpace(c.Query("sort_order"))),
	}
	if raw := strings.TrimSpace(c.Query("port")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 0 || port > 65535 {
			return filter, &assetQueryError{field: "port", value: raw}
		}
		filter.Port = &port
	}
	var err error
	if filter.LastScanBefore, err = parseAssetQueryTime("last_scan_before", c.Query("last_scan_before")); err != nil {
		return filter, err
	}
	if filter.LastScanAfter, err = parseAssetQueryTime("last_scan_after", c.Query("last_scan_after")); err != nil {
		return filter, err
	}
	if filter.LastSeenBefore, err = parseAssetQueryTime("last_seen_before", c.Query("last_seen_before")); err != nil {
		return filter, err
	}
	if filter.LastSeenAfter, err = parseAssetQueryTime("last_seen_after", c.Query("last_seen_after")); err != nil {
		return filter, err
	}
	return filter, nil
}

type assetQueryError struct{ field, value string }

func (e *assetQueryError) Error() string {
	return e.field + " 参数无效: " + e.value
}

func parseAssetQueryTime(field, value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return &parsed, nil
		}
	}
	return nil, &assetQueryError{field: field, value: value}
}

func (h *AssetHandler) Stats(c *gin.Context) {
	days := 30
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || (parsed != 7 && parsed != 30 && parsed != 90) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "days 仅支持 7、30 或 90"})
			return
		}
		days = parsed
	}
	stats, err := h.db.GetAssetStats(assetAccess(c), days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, stats)
}

// RecordScans stores the execution link created by the asset-library scan action.
func (h *AssetHandler) RecordScans(c *gin.Context) {
	var req recordAssetScansRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.Scans) == 0 || len(req.Scans) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scans 数量必须在 1-1000 之间"})
		return
	}
	access := assetAccess(c)
	for _, scan := range req.Scans {
		conversationID := strings.TrimSpace(scan.ConversationID)
		queueID := strings.TrimSpace(scan.QueueID)
		taskID := strings.TrimSpace(scan.TaskID)
		if conversationID == "" && taskID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "conversation_id 或 task_id 至少需要一个"})
			return
		}
		if taskID != "" && (queueID == "" || !h.db.BatchTaskBelongsToQueue(taskID, queueID)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "任务不属于指定队列"})
			return
		}
		if _, err := h.db.GetAsset(strings.TrimSpace(scan.AssetID), access); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "资产不存在或无权扫描"})
			return
		}
		if session, ok := security.CurrentSession(c); ok {
			if id := conversationID; id != "" && !h.db.UserCanAccessResource(session.UserID, session.Scope, "conversation", id) {
				c.JSON(http.StatusForbidden, gin.H{"error": "无权关联该对话"})
				return
			}
			if id := queueID; id != "" && !h.db.UserCanAccessResource(session.UserID, session.Scope, "batch_task", id) {
				c.JSON(http.StatusForbidden, gin.H{"error": "无权关联该任务队列"})
				return
			}
		}
	}
	for _, scan := range req.Scans {
		if err := h.db.MarkAssetScanned(scan.AssetID, scan.ConversationID, scan.QueueID, scan.TaskID, access); err != nil {
			h.logger.Error("记录资产扫描失败", zap.String("asset_id", scan.AssetID), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"updated": len(req.Scans)})
}

func (h *AssetHandler) Update(c *gin.Context) {
	var asset database.Asset
	if err := c.ShouldBindJSON(&asset); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if asset.ProjectID != "" {
		if session, ok := security.CurrentSession(c); ok && !h.db.UserCanAccessResource(session.UserID, session.Scope, "project", asset.ProjectID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "无权绑定该项目"})
			return
		}
	}
	if err := h.db.UpdateAsset(c.Param("id"), &asset, assetAccess(c)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, err := h.db.GetAsset(c.Param("id"), assetAccess(c))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "资产不存在"})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// UpdateProjectBinding replaces the project binding for a selected asset set.
func (h *AssetHandler) UpdateProjectBinding(c *gin.Context) {
	var req updateAssetsProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(req.AssetIDs) == 0 || len(req.AssetIDs) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "asset_ids 数量必须在 1-1000 之间"})
		return
	}
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	if req.ProjectID != "" {
		if _, err := h.db.GetProject(req.ProjectID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "项目不存在"})
			return
		}
		if session, ok := security.CurrentSession(c); ok && !h.db.UserCanAccessResource(session.UserID, session.Scope, "project", req.ProjectID) {
			c.JSON(http.StatusForbidden, gin.H{"error": "无权绑定该项目"})
			return
		}
	}
	updated, err := h.db.UpdateAssetsProject(req.AssetIDs, req.ProjectID, assetAccess(c))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"updated": updated, "project_id": req.ProjectID})
}

func (h *AssetHandler) Delete(c *gin.Context) {
	if err := h.db.DeleteAsset(c.Param("id"), assetAccess(c)); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "资产不存在或无权删除"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}
