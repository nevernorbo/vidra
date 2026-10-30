package views

import (
	"fmt"
	"time"
)

func FormatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f, i := float64(n)/1024, 0
	for f >= 1024 && i < 3 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %cB", f, "KMGT"[i])
}

func fmtExpires(t int64) string {
	if t == 0 {
		return "Never expires"
	}
	d := time.Until(time.Unix(t, 0))
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("Expires in %dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("Expires in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("Expires in %dm", max(int(d.Minutes()), 1))
	}
}

// duration spells out the time left until t for prose, rounded so a fresh 7-day invite reads "7 days".
func duration(t int64) string {
	d := time.Until(time.Unix(t, 0))
	n, unit := int(d.Round(24*time.Hour).Hours()/24), "day"
	switch {
	case d < time.Hour:
		n, unit = max(int(d.Round(time.Minute).Minutes()), 1), "minute"
	case d < 48*time.Hour:
		n, unit = int(d.Round(time.Hour).Hours()), "hour"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

func fmtDate(t int64) string { return time.Unix(t, 0).Format("2 Jan 2006") }
