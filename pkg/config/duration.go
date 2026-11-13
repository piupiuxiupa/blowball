package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// regexpMustCompile is a thin wrapper so the top of config.go reads cleanly.
func regexpMustCompile(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
}

// parseDuration accepts a short-form duration string with a trailing unit
// suffix (s, m, h, d, w) or any value understood by time.ParseDuration.
//
// Examples: "30s", "15m", "24h", "7d", "2w".
func parseDuration(s string) (time.Duration, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("empty duration")
	}

	// Single trailing unit suffix form, e.g. "7d", "2w".
	suffix := raw[len(raw)-1]
	multiplier, isShort := shortUnitMultipliers[suffix]
	if isShort {
		numStr := raw[:len(raw)-1]
		n, err := strconv.ParseFloat(numStr, 64)
		if err == nil {
			return time.Duration(n * float64(multiplier)), nil
		}
		// fall through to standard parser
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return d, nil
}

var shortUnitMultipliers = map[byte]time.Duration{
	's': time.Second,
	'm': time.Minute,
	'h': time.Hour,
	'd': 24 * time.Hour,
	'w': 7 * 24 * time.Hour,
}
