package admin

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// ListCodexTurnStateHistory returns paged renewal outcomes without template values.
func (h *Handler) ListCodexTurnStateHistory(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		writeError(c, http.StatusBadRequest, "页码超出范围")
		return
	}
	filter := database.CodexTurnStateHistoryFilter{Plan: strings.TrimSpace(c.Query("plan")), Model: strings.TrimSpace(c.Query("model")), Status: c.Query("status"), ProxyURL: c.Query("proxy_url")}
	switch filter.Status {
	case "", "running", "success", "failed", "interrupted":
	default:
		writeError(c, http.StatusBadRequest, "无效的续签状态")
		return
	}
	if raw := c.Query("account_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(c, http.StatusBadRequest, "无效的账号 ID")
			return
		}
		filter.AccountID = id
	}
	result, err := h.db.ListCodexTurnStateHistory(c.Request.Context(), page, size, filter)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取续签记录失败")
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) startCodexTurnStateHistory(ctx context.Context, account *auth.Account, row database.CodexTurnStateTemplate, attempt int, route codexTurnStateRenewalProxy, started time.Time) int64 {
	account.Mu().RLock()
	name, plan := account.Email, account.PlanType
	account.Mu().RUnlock()
	if name == "" {
		name = fmt.Sprintf("ID %d", account.ID())
	}
	if saved, err := h.db.GetAccountByID(ctx, account.ID()); err == nil && saved != nil && saved.Name != "" {
		name = saved.Name
	}
	record := database.CodexTurnStateRenewalRecord{AccountID: account.ID(), AccountName: name, PlanType: plan, Model: row.Model, Attempt: attempt, MaxAttempts: database.CodexTurnStateRenewalMaxAttempts, ProxyID: route.id, ProxyName: route.name, ProxyURL: route.displayURL, ProxyIP: route.ip, Route: route.source, StartedAt: started.UnixMilli(), ExpiresBefore: proxy.CodexTurnStateTemplateExpiresAt(row.IssuedAt).UnixMilli()}
	id, err := h.db.StartCodexTurnStateHistory(ctx, record)
	if err != nil {
		log.Printf("[turn-state-renewal] history start failed account=%d: %v", account.ID(), err)
	}
	return id
}
