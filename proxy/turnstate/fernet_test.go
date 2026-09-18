package turnstate

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"
)

func fakeFernet(issued int64, cipherBytes int) string {
	raw := make([]byte, 1+8+16+cipherBytes+32)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued))
	return base64.URLEncoding.EncodeToString(raw)
}

func TestParseFernetAndInjectible(t *testing.T) {
	now := time.Now().Unix()
	tok := fakeFernet(now, 160)
	parsed, ok := ParseFernet(tok)
	if !ok {
		t.Fatal("expected parse")
	}
	if parsed.Blocks != 10 || parsed.IssuedUnix != now {
		t.Fatalf("parsed=%+v", parsed)
	}
	if parsed.Length != FullBloodChars {
		t.Fatalf("10-block encoding length = %d, want %d (adjust fixture)", parsed.Length, FullBloodChars)
	}
	if _, ok := Injectible(tok, now); !ok {
		t.Fatal("fresh 10-block token should inject")
	}
	if _, ok := Injectible(fakeFernet(now, 176), now); ok {
		t.Fatal("11-block token must not inject")
	}
	if _, ok := Injectible(fakeFernet(now-4000, 160), now); ok {
		t.Fatal("expired token must not inject")
	}
}

func TestCacheLookupRespectsSwitchAndModel(t *testing.T) {
	c := NewCache()
	now := time.Now().Unix()
	tok := fakeFernet(now, 160)
	parsed, ok := ParseFernet(tok)
	if !ok || parsed.Length != FullBloodChars {
		t.Fatalf("fixture length=%d ok=%v", parsed.Length, ok)
	}
	c.Put(CachedTicket{AccountID: 1, Model: "gpt-6-astra", Token: tok, IssuedUnix: parsed.IssuedUnix, Length: parsed.Length, Blocks: parsed.Blocks})
	SetConfig(DefaultConfig())
	if got := c.LookupInjectible(1, "gpt-6-astra", now); got != "" {
		t.Fatal("switch off must not inject")
	}
	SetConfig(Config{InjectEnabled: true})
	t.Cleanup(func() { SetConfig(DefaultConfig()) })
	if got := c.LookupInjectible(1, "gpt-6-astra", now); got != tok {
		t.Fatalf("got %q", got)
	}
	if got := c.LookupInjectible(1, "gpt-5.6-sol", now); got != "" {
		t.Fatalf("other model must miss, got %q", got)
	}
}
