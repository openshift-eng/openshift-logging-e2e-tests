package strings

// TruncateForLogs keeps failure messages readable
func TruncateForLogs(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "...(truncated)"
}
