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

func fmtDate(t int64) string { return time.Unix(t, 0).Format("2 Jan 2006") }
