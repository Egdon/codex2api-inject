package proxy

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/database"
)

// Production uses the database as the only template store. Observations contain
// no tokens and remain bounded process memory; entries is the isolated test fallback.
func SetCodexTurnStateTemplateDatabase(db *database.DB) {
	s := globalTurnStateTemplates
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = db
	s.entries = make(map[turnStateTemplateKey]turnStateTemplateEntry)
}

func templateDBContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}

func (s *turnStateTemplateStore) entryLocked(key turnStateTemplateKey) (turnStateTemplateEntry, bool) {
	if s.db == nil {
		v, ok := s.entries[key]
		return v, ok
	}
	ctx, cancel := templateDBContext()
	defer cancel()
	row, ok, err := s.db.GetCodexTurnStateTemplate(ctx, key.AccountID, key.Model)
	if err != nil {
		log.Print("[codex-turn-state] database read failed")
		return turnStateTemplateEntry{}, false
	}
	return turnStateTemplateEntry{Value: row.Value, IssuedAt: time.Unix(row.IssuedAt, 0), Strikes: row.Strikes}, ok
}

func (s *turnStateTemplateStore) saveEntryLocked(key turnStateTemplateKey, entry turnStateTemplateEntry) bool {
	if s.db == nil {
		s.entries[key] = entry
		return true
	}
	ctx, cancel := templateDBContext()
	defer cancel()
	err := s.db.SaveCodexTurnStateTemplate(ctx, database.CodexTurnStateTemplate{AccountID: key.AccountID, Model: key.Model, Value: entry.Value, IssuedAt: entry.IssuedAt.Unix(), Strikes: entry.Strikes, UpdatedAt: s.now().Unix()})
	if err != nil {
		log.Print("[codex-turn-state] database save failed")
		return false
	}
	return true
}

func (s *turnStateTemplateStore) deleteEntryLocked(key turnStateTemplateKey) {
	if s.db == nil {
		delete(s.entries, key)
		return
	}
	ctx, cancel := templateDBContext()
	defer cancel()
	if s.db.DeleteCodexTurnStateTemplate(ctx, key.AccountID, key.Model) != nil {
		log.Print("[codex-turn-state] database delete failed")
	}
}

func (s *turnStateTemplateStore) accountEntriesLocked(accountID int64) map[turnStateTemplateKey]turnStateTemplateEntry {
	result := make(map[turnStateTemplateKey]turnStateTemplateEntry)
	if s.db == nil {
		for k, v := range s.entries {
			if k.AccountID == accountID {
				result[k] = v
			}
		}
		return result
	}
	ctx, cancel := templateDBContext()
	defer cancel()
	rows, err := s.db.ListCodexTurnStateTemplates(ctx, accountID)
	if err != nil {
		log.Print("[codex-turn-state] database list failed")
		return result
	}
	for _, row := range rows {
		result[turnStateTemplateKey{AccountID: row.AccountID, Model: row.Model}] = turnStateTemplateEntry{Value: row.Value, IssuedAt: time.Unix(row.IssuedAt, 0), Strikes: row.Strikes}
	}
	return result
}
