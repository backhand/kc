package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/thinkpilot/infrastructure/tools/kc/internal/k8s"
)

// formatCPU renders millicores compactly: "250m", "1.50" (cores) once ≥ 1000m.
func formatCPU(millicores int64) string {
	if millicores < 1000 {
		return fmt.Sprintf("%dm", millicores)
	}
	return fmt.Sprintf("%.2f", float64(millicores)/1000)
}

// formatMem renders bytes in binary units (Mi/Gi), the units k8s capacity uses.
func formatMem(bytes int64) string {
	const (
		ki = 1 << 10
		mi = 1 << 20
		gi = 1 << 30
	)
	switch {
	case bytes >= gi:
		return fmt.Sprintf("%.1fGi", float64(bytes)/gi)
	case bytes >= mi:
		return fmt.Sprintf("%dMi", bytes/mi)
	case bytes >= ki:
		return fmt.Sprintf("%dKi", bytes/ki)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// formatUsage renders a usage sample as "cpu / mem".
func formatUsage(u k8s.Usage) string {
	return formatCPU(u.CPUMillicores) + " / " + formatMem(u.MemoryBytes)
}

// usageCell renders an optional usage pointer; nil ("no metrics") becomes a dash
// so a missing metrics-server reads clearly rather than as a spurious zero.
func usageCell(u *k8s.Usage) string {
	if u == nil {
		return "—"
	}
	return formatUsage(*u)
}

// formatAge renders a duration as a compact relative age: "3s", "5m", "2h",
// "4d". Used both for freshness ("updated 8s ago") and elsewhere.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		s := int(d.Seconds())
		if s < 1 {
			s = 0
		}
		return fmt.Sprintf("%ds", s)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// barGlyphs renders a fixed-width fill gauge for a fraction in [0,1] using block
// glyphs, e.g. used/capacity. width is the inner cell count.
func barGlyphs(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// truncate trims s to max runes, appending "…" when it had to cut. A max ≤ 0
// returns "".
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

// pad right-pads s with spaces to width runes (no truncation — callers truncate
// first when a hard cap matters).
func pad(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// leftPad left-pads s with spaces to width runes (no truncation). Used to align
// the right edge of the gauge value text so the bars that follow start at a
// fixed column across rows.
func leftPad(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return strings.Repeat(" ", width-n) + s
}
