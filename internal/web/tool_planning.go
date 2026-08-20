package web

import "strings"

func toolPlanningMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "router":
		return "router"
	default:
		return "native"
	}
}
