package builtin

import (
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func mapDecision(d permission.Decision) tools.Permission {
	switch d {
	case permission.DecisionAllow:
		return tools.PermissionAllow
	case permission.DecisionDeny:
		return tools.PermissionDeny
	default:
		return tools.PermissionAsk
	}
}

func strFromAny(v any) string {
	if v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		return fmt.Sprintf("%v", v)
	}
}

func intArg(in map[string]any, key string, def int) int {
	if v, ok := in[key]; ok {
		switch x := v.(type) {
		case int:
			return x
		case int64:
			return int(x)
		case float64:
			return int(x)
		}
	}
	return def
}

func intStr(i int) string { return fmt.Sprintf("%d", i) }

func bytesString(n int) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
