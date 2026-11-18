// Package util holds the forge's small shared helpers.
//
// Everything time-like here is deterministic: the trial hands the entry
// MAKEFUN_TRIAL_NOW (an ISO timestamp) and the forge never reads the wall
// clock for anything it returns. When the variable is absent (a local
// boot), a fixed epoch is used so a local run is reproducible too.
package util

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// defaultNow is the fixed "now" used when MAKEFUN_TRIAL_NOW is absent or
// unparsable, matching the Node entry's fallback.
const defaultNow = "2023-11-14T22:13:20.000Z"

// trialNow parses MAKEFUN_TRIAL_NOW once per process. The trial timestamp
// is an ISO-8601 string; anything unparsable falls back to defaultNow.
var trialNow = sync.OnceValue(func() time.Time {
	raw := os.Getenv("MAKEFUN_TRIAL_NOW")
	if raw != "" {
		for _, layout := range []string{
			time.RFC3339Nano,
			"2006-01-02T15:04:05.999999999Z07:00",
			"2006-01-02T15:04:05.999999999",
			"2006-01-02 15:04:05.999999999Z07:00",
		} {
			if t, err := time.Parse(layout, raw); err == nil {
				return t.UTC()
			}
		}
	}
	t, _ := time.Parse(time.RFC3339Nano, defaultNow)
	return t.UTC()
})

// TrialNowISO returns the trial's "now" as an ISO string with millisecond
// precision and a Z suffix, the way JavaScript's Date.toISOString spells it.
func TrialNowISO() string {
	return trialNow().Format("2006-01-02T15:04:05.000Z")
}

// TrialNowEpoch returns the trial's "now" as epoch seconds, for git commit
// timestamps.
func TrialNowEpoch() int64 {
	return trialNow().Unix()
}

// SHA1Hex returns the SHA-1 hex of data.
func SHA1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// SHA1Digest returns the SHA-1 digest of data as raw bytes (pack trailers).
func SHA1Digest(data []byte) [20]byte {
	return sha1.Sum(data)
}

// B64EncodeText returns the standard base64 of UTF-8 text.
func B64EncodeText(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

// B64EncodeBytes returns the standard base64 of data.
func B64EncodeBytes(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// B64Decode decodes base64, ignoring the whitespace GitHub wraps long
// values at.
func B64Decode(content string) ([]byte, error) {
	stripped := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, content)
	return base64.StdEncoding.DecodeString(stripped)
}

var oidPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsOID reports whether value is a 40-character lowercase hex object id.
func IsOID(value string) bool {
	return oidPattern.MatchString(value)
}

// ZeroOID is the all-zero object id.
const ZeroOID = "0000000000000000000000000000000000000000"

// requestCounter backs NextRequestID; it is per-process and deterministic
// in request order.
var requestCounter int64

// NextRequestID returns the next "req-N" id for X-GitHub-Request-Id.
func NextRequestID() string {
	return fmt.Sprintf("req-%d", atomic.AddInt64(&requestCounter, 1))
}

// SafeText renders v as text with control characters blanked out and
// truncated to max runes, so no control character leaks into a JSON error
// message. A nil value renders as "null", matching JavaScript's String().
func SafeText(v any, max int) string {
	var text string
	switch t := v.(type) {
	case nil:
		text = "null"
	case string:
		text = t
	case []byte:
		text = string(t)
	default:
		text = fmt.Sprint(t)
	}
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, text)
	if max < 0 {
		max = 0
	}
	runes := []rune(cleaned)
	if len(runes) > max {
		runes = runes[:max]
	}
	return string(runes)
}
