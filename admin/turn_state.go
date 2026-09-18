package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/turnstate"
	"github.com/gin-gonic/gin"
)

func (h *Handler) SetTurnStateHarvest(harvester *turnstate.Harvester) {
	h.turnStateHarvest = harvester
}

func (h *Handler) GetTurnStateSettings(c *gin.Context) {
	c.JSON(http.StatusOK, turnstate.Publicize(turnstate.GetConfig()))
}

func (h *Handler) UpdateTurnStateSettings(c *gin.Context) {
	var raw map[string]json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	buf, _ := json.Marshal(raw)
	var req turnstate.Config
	if err := json.Unmarshal(buf, &req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if h.turnStateHarvest == nil {
		writeError(c, http.StatusServiceUnavailable, "探测未启动")
		return
	}
	normalized, err := h.turnStateHarvest.SaveConfig(c.Request.Context(), req,
		strings.TrimSpace(req.ZooPassword) == "", raw["disabled_account_ids"] == nil)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, turnstate.Publicize(normalized))
}

type turnStateAccountRow struct {
	ID             int64                     `json:"id"`
	Email          string                    `json:"email"`
	PlanType       string                    `json:"plan_type"`
	HarvestEnabled bool                      `json:"harvest_enabled"`
	Tickets        []turnStateTicketView     `json:"tickets"`
	AstraPolicy    database.AstraPolicyState `json:"astra_policy"`
}

type turnStateTicketView struct {
	Model          string `json:"model"`
	Class          string `json:"class"`
	Length         int    `json:"length"`
	Blocks         int    `json:"blocks"`
	IssuedUnix     int64  `json:"issued_unix"`
	RemainingSec   int64  `json:"remaining_sec"`
	ConfirmWarning bool   `json:"confirm_warning"`
	Exhausted      bool   `json:"exhausted"`
	Attempts       int    `json:"attempts"`
	LastError      string `json:"last_error"`
	LastHarvestAt  int64  `json:"last_harvest_at"`
	CooldownUntil  int64  `json:"cooldown_until"`
	Token          string `json:"token"`
}

func (h *Handler) GetTurnStateOverview(c *gin.Context) {
	cfg := turnstate.GetConfig()
	now := time.Now().Unix()
	accounts := make([]turnStateAccountRow, 0)
	if h.turnStateHarvest != nil {
		for _, acc := range filterInjectPageAccounts(h.store.Accounts()) {
			row := turnStateAccountRow{
				ID:             acc.ID(),
				Email:          accountEmail(acc),
				PlanType:       acc.GetPlanType(),
				HarvestEnabled: turnstate.AccountHarvestEnabled(cfg, acc.ID()),
				Tickets:        make([]turnStateTicketView, 0, len(cfg.Models)),
				AstraPolicy:    h.turnStateHarvest.PolicySnapshot(acc.ID()),
			}
			for _, model := range cfg.Models {
				view := turnStateTicketView{Model: model, Class: "none"}
				if t, ok := turnstate.Global().Get(acc.ID(), model); ok {
					view.Class = turnstate.ClassOf(t.Length)
					view.Length = t.Length
					view.Blocks = t.Blocks
					view.IssuedUnix = t.IssuedUnix
					view.RemainingSec = turnstate.RemainingSeconds(t.IssuedUnix, now)
					view.ConfirmWarning = t.ConfirmWarning
					view.Exhausted = t.Exhausted
					view.Attempts = t.Attempts
					view.LastError = t.LastError
					view.LastHarvestAt = t.LastHarvestAt
					view.CooldownUntil = t.CooldownUntil
					view.Token = t.Token
				}
				row.Tickets = append(row.Tickets, view)
			}
			accounts = append(accounts, row)
		}
	}
	job := (*turnstate.Job)(nil)
	var breaker any
	if h.turnStateHarvest != nil {
		job = h.turnStateHarvest.CurrentJob()
		breaker = h.turnStateHarvest.BreakerSnapshot()
	}
	c.JSON(http.StatusOK, gin.H{
		"config":   turnstate.Publicize(cfg),
		"accounts": accounts,
		"job":      job,
		"breaker":  breaker,
		"now_unix": now,
	})
}

func filterInjectPageAccounts(accounts []*auth.Account) []*auth.Account {
	out := make([]*auth.Account, 0)
	for _, acc := range accounts {
		if acc == nil || acc.ID() <= 0 {
			continue
		}
		if atomic.LoadInt32(&acc.Disabled) != 0 {
			continue
		}
		acc.Mu().RLock()
		upstream := strings.TrimSpace(acc.UpstreamType)
		acc.Mu().RUnlock()
		if strings.EqualFold(upstream, auth.UpstreamGrok) ||
			strings.EqualFold(upstream, auth.UpstreamClaude) ||
			strings.EqualFold(upstream, auth.UpstreamAntigravity) {
			continue
		}
		out = append(out, acc)
	}
	return out
}

func accountEmail(acc *auth.Account) string {
	if acc == nil {
		return ""
	}
	acc.Mu().RLock()
	defer acc.Mu().RUnlock()
	return strings.TrimSpace(acc.Email)
}

func (h *Handler) PostTurnStateHarvest(c *gin.Context) {
	if h.turnStateHarvest == nil {
		writeError(c, http.StatusServiceUnavailable, "探测未启动")
		return
	}
	var req struct {
		AccountID  int64   `json:"account_id"`
		AccountIDs []int64 `json:"account_ids"`
		Model      string  `json:"model"`
		Force      bool    `json:"force"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	var job *turnstate.Job
	var err error
	if req.AccountIDs != nil {
		if req.AccountID != 0 || len(req.AccountIDs) == 0 {
			writeError(c, http.StatusBadRequest, "account_ids 不能为空且不可与 account_id 同时使用")
			return
		}
		job, err = h.turnStateHarvest.EnqueueSelected(req.AccountIDs, req.Model, req.Force)
	} else {
		job, err = h.turnStateHarvest.Enqueue(req.AccountID, req.Model, req.Force || req.AccountID > 0)
	}
	if err != nil {
		writeError(c, http.StatusConflict, err.Error())
		return
	}
	c.JSON(http.StatusAccepted, job)
}

func (h *Handler) PostTurnStateHarvestCancel(c *gin.Context) {
	if h.turnStateHarvest != nil {
		h.turnStateHarvest.CancelJob()
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) PutTurnStateTicket(c *gin.Context) {
	if h.turnStateHarvest == nil {
		writeError(c, http.StatusServiceUnavailable, "探测未启动")
		return
	}
	accountID, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	model := strings.TrimSpace(c.Param("model"))
	if accountID <= 0 || model == "" {
		writeError(c, http.StatusBadRequest, "账号或模型无效")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	t, err := h.turnStateHarvest.ManualPaste(c.Request.Context(), accountID, model, req.Token)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, t)
}

func (h *Handler) DeleteTurnStateTicket(c *gin.Context) {
	if h.turnStateHarvest == nil {
		writeError(c, http.StatusServiceUnavailable, "探测未启动")
		return
	}
	accountID, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	model := strings.TrimSpace(c.Param("model"))
	if err := h.turnStateHarvest.Clear(c.Request.Context(), accountID, model); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) PutTurnStateAccountHarvest(c *gin.Context) {
	if h.turnStateHarvest == nil {
		writeError(c, http.StatusServiceUnavailable, "探测未启动")
		return
	}
	accountID, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		writeError(c, http.StatusBadRequest, "enabled 必须为布尔值")
		return
	}
	cfg, err := h.turnStateHarvest.SetHarvestParticipationBatch(c.Request.Context(), []int64{accountID}, *req.Enabled)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, turnstate.Publicize(cfg))
}

func (h *Handler) PutTurnStateAccountsHarvest(c *gin.Context) {
	if h.turnStateHarvest == nil {
		writeError(c, http.StatusServiceUnavailable, "探测未启动")
		return
	}
	var req struct {
		AccountIDs []int64 `json:"account_ids"`
		Enabled    *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Enabled == nil {
		writeError(c, http.StatusBadRequest, "account_ids 和 enabled 参数无效")
		return
	}
	cfg, err := h.turnStateHarvest.SetHarvestParticipationBatch(c.Request.Context(), req.AccountIDs, *req.Enabled)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, turnstate.Publicize(cfg))
}

func (h *Handler) ServeInjectPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	// Independent page with inline JS/CSS. Override the global admin CSP that
	// only allows the SPA theme-restore hash; otherwise the login button is a no-op.
	c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https: http:; font-src 'self'; connect-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
	c.Data(http.StatusOK, "text/html; charset=utf-8", injectPageHTML)
}
