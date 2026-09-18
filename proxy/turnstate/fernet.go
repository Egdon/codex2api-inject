package turnstate

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"unicode/utf8"
)

const (
	FernetTTLSeconds  = int64(3600)
	FullBloodChars    = 292
	DegradedChars     = 312
	FullBloodBlocks   = 10
	fernetVersion     = 0x80
	fernetIssuedFloor = int64(1_577_836_800)
	fernetIssuedCeil  = int64(4_102_444_800)
	maxTurnStateBytes = 4096
)

type Parsed struct {
	IssuedUnix int64
	Length     int
	Blocks     int
	Cipher     int
}

func NormalizeToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxTurnStateBytes || !utf8.ValidString(value) {
		return ""
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return ""
		}
	}
	return value
}

func ParseFernet(token string) (Parsed, bool) {
	token = NormalizeToken(token)
	if token == "" {
		return Parsed{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
	if err != nil {
		padded := token
		switch len(token) % 4 {
		case 2:
			padded += "=="
		case 3:
			padded += "="
		}
		raw, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return Parsed{}, false
		}
	}
	if len(raw) < 1+8+16+32 || raw[0] != fernetVersion {
		return Parsed{}, false
	}
	cipher := len(raw) - 1 - 8 - 16 - 32
	if cipher < 16 || cipher%16 != 0 {
		return Parsed{}, false
	}
	issued := int64(binary.BigEndian.Uint64(raw[1:9]))
	if issued < fernetIssuedFloor || issued >= fernetIssuedCeil {
		return Parsed{}, false
	}
	return Parsed{
		IssuedUnix: issued,
		Length:     len(token),
		Blocks:     cipher / 16,
		Cipher:     cipher,
	}, true
}

func ClassOf(length int) string {
	switch length {
	case FullBloodChars:
		return "short_292"
	case DegradedChars:
		return "long_312"
	case 0:
		return "none"
	default:
		return "other"
	}
}

func RemainingSeconds(issuedUnix, nowUnix int64) int64 {
	if issuedUnix <= 0 {
		return 0
	}
	return issuedUnix + FernetTTLSeconds - nowUnix
}

func Injectible(token string, nowUnix int64) (string, bool) {
	token = NormalizeToken(token)
	if token == "" {
		return "", false
	}
	parsed, ok := ParseFernet(token)
	if !ok || parsed.Length != FullBloodChars {
		return "", false
	}
	if RemainingSeconds(parsed.IssuedUnix, nowUnix) <= 0 {
		return "", false
	}
	return token, true
}
