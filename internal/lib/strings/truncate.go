package strings

// TruncateForLogs keeps failure messages readable
func TruncateForLogs(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
