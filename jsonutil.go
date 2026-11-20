package ccleft

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// flexNum decodes a JSON number, a numeric string, or null. Upstream schemas
// drift between the two (codex credits.balance is "0" on one day and 0 on
// another); a fixed Go type turns that into a hard parse failure.
type flexNum struct {
	V  float64
	OK bool
}

func (f *flexNum) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*f = flexNum{}
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			*f = flexNum{}
			return nil
		}
		v, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return fmt.Errorf("flexNum: %q is not numeric", str)
		}
		*f = flexNum{V: v, OK: true}
		return nil
	}
	if s == "true" || s == "false" {
		return fmt.Errorf("flexNum: boolean %s is not numeric", s)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexNum{V: v, OK: true}
	return nil
}

func (f flexNum) ptr() *float64 {
	if !f.OK {
		return nil
	}
	return f64(f.V)
}

// parseTime accepts RFC3339 strings, epoch seconds or epoch milliseconds.
func parseTime(v any) *time.Time {
	var t time.Time
	switch x := v.(type) {
	case string:
		x = strings.TrimSpace(x)
		if x == "" {
			return nil
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
			if pt, err := time.Parse(layout, x); err == nil {
				t = pt
				break
			}
		}
		if t.IsZero() {
			if n, err := strconv.ParseFloat(x, 64); err == nil {
				return parseTime(n)
			}
			return nil
		}
	case float64:
		if x <= 0 {
			return nil
		}
		if x > 1e12 {
			t = time.UnixMilli(int64(x))
		} else {
			sec := int64(x)
			t = time.Unix(sec, int64((x-float64(sec))*1e9))
		}
	case int64:
		return parseTime(float64(x))
	case flexNum:
		if !x.OK {
			return nil
		}
		return parseTime(x.V)
	default:
		return nil
	}
	t = t.UTC()
	return &t
}
