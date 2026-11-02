package reginfo

import (
	"strings"
	"time"
)

func parseTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05", "2006-01-02", "02-Jan-2006", "2006.01.02", "02/01/2006", "2006/01/02"}
	for _, l := range layouts {
		if t, e := time.Parse(l, s); e == nil {
			return &t
		}
	}
	return nil
}
func ageDays(t *time.Time) *int {
	if t == nil {
		return nil
	}
	n := int(time.Since(*t).Hours() / 24)
	return &n
}
func uniq(xs []string) []string {
	m := map[string]bool{}
	o := []string{}
	for _, x := range xs {
		x = strings.TrimSpace(strings.TrimSuffix(x, "."))
		if x != "" && !m[strings.ToLower(x)] {
			m[strings.ToLower(x)] = true
			o = append(o, x)
		}
	}
	return o
}
