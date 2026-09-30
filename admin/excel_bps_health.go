package admin

import (
	"net/http"
	"strconv"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// ClearAccountExcelBPSPause resumes an account's Excel Basispoints route after
// an automatic 403 pause or a 429 cooldown, without waiting for a probe.
func (h *Handler) ClearAccountExcelBPSPause(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	cleared := proxy.ClearExcelBPSPause(id)
	c.JSON(http.StatusOK, gin.H{"message": "Excel BPS 路由已恢复", "cleared": cleared})
}

// excelBPSPauseForAccount reports an account's Basispoints route health for
// the account list; nil means the route is healthy.
func excelBPSPauseForAccount(accountID int64) *proxy.ExcelBPSPauseView {
	return proxy.ExcelBPSPauseFor(accountID)
}
