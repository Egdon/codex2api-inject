package turnstate

import (
	"strings"
	"sync"
	"time"
)

type CachedTicket struct {
	AccountID      int64
	Model          string
	Token          string
	IssuedUnix     int64
	Length         int
	Blocks         int
	ConfirmWarning bool
	Exhausted      bool
	Attempts       int
	LastError      string
	LastHarvestAt  int64
	CooldownUntil  int64
}

type Cache struct {
	mu      sync.RWMutex
	tickets map[string]CachedTicket
}

func NewCache() *Cache {
	return &Cache{tickets: make(map[string]CachedTicket)}
}

func ticketKey(accountID int64, model string) string {
	return strings.ToLower(strings.TrimSpace(model)) + "\x00" + itoa64(accountID)
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func (c *Cache) ReplaceAll(tickets []CachedTicket) {
	next := make(map[string]CachedTicket, len(tickets))
	for _, t := range tickets {
		t.Model = strings.TrimSpace(t.Model)
		if t.AccountID <= 0 || t.Model == "" {
			continue
		}
		next[ticketKey(t.AccountID, t.Model)] = t
	}
	c.mu.Lock()
	c.tickets = next
	c.mu.Unlock()
}

func (c *Cache) Put(t CachedTicket) {
	t.Model = strings.TrimSpace(t.Model)
	if t.AccountID <= 0 || t.Model == "" {
		return
	}
	c.mu.Lock()
	c.tickets[ticketKey(t.AccountID, t.Model)] = t
	c.mu.Unlock()
}

func (c *Cache) Delete(accountID int64, model string) {
	c.mu.Lock()
	delete(c.tickets, ticketKey(accountID, model))
	c.mu.Unlock()
}

func (c *Cache) Get(accountID int64, model string) (CachedTicket, bool) {
	c.mu.RLock()
	t, ok := c.tickets[ticketKey(accountID, model)]
	c.mu.RUnlock()
	return t, ok
}

func (c *Cache) Snapshot() []CachedTicket {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CachedTicket, 0, len(c.tickets))
	for _, t := range c.tickets {
		out = append(out, t)
	}
	return out
}

// LookupInjectible is the hot path: no Fernet decode, Unix compare only.
func (c *Cache) LookupInjectible(accountID int64, model string, nowUnix int64) string {
	if !GetConfig().InjectEnabled || accountID <= 0 {
		return ""
	}
	t, ok := c.Get(accountID, model)
	if !ok {
		return ""
	}
	if t.Length != FullBloodChars || t.IssuedUnix <= 0 || RemainingSeconds(t.IssuedUnix, nowUnix) <= 0 {
		return ""
	}
	return t.Token
}

func (c *Cache) LookupInjectibleNow(accountID int64, model string) string {
	return c.LookupInjectible(accountID, model, time.Now().Unix())
}

var globalCache = NewCache()

func Global() *Cache { return globalCache }
