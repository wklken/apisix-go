package util

// TerminalStatus reports whether code is a Go-terminal HTTP status (200..599).
// Informational 1xx codes are rejected so they cannot be written as an interim
// response followed by an implicit 200.
func TerminalStatus(code int) (int, bool) {
	if code < 200 || code > 599 {
		return 0, false
	}
	return code, true
}
