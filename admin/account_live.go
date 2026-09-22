package admin

import (
	"net/http"

	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

type accountLiveItem struct {
	CodexTurnStateStatus *proxy.CodexTurnStateStatus `json:"codex_turn_state_status,omitempty"`
	ActiveRequests       int64                       `json:"active_requests"`
	OccupiedRequests     int64                       `json:"occupied_requests"`
}

// GetAccountLiveState returns request-local runtime counters for the visible
// account page. Scheduler counters use in-memory atomics; Turn-State templates
// are read from the database without rebuilding the paged snapshot.
func (h *Handler) GetAccountLiveState(c *gin.Context) {
	ids, err := parseAccountListIDs(c.Query("ids"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "ids 参数无效")
		return
	}
	if len(ids) > accountListPageMax {
		writeError(c, http.StatusBadRequest, "ids 最多允许 500 个")
		return
	}

	live := make(map[int64]accountLiveItem, len(ids))
	for _, id := range ids {
		account := h.store.FindByID(id)
		if account == nil {
			continue
		}
		live[id] = accountLiveItem{
			CodexTurnStateStatus: proxy.GetCodexTurnStateStatus(account),
			ActiveRequests:       account.GetActiveRequests(),
			OccupiedRequests:     account.GetOccupiedRequests(),
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"accounts":                    live,
		"session_slot_buffer_enabled": h.store.SessionSlotBufferEnabled(),
	})
}
