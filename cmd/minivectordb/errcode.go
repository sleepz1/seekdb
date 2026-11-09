package main

import "strings"

// errorCode maps an engine error message to a stable, non-empty error code.
// The public test pack asserts errors by semantic name (duplicate_key,
// unsupported_sql, dimension_mismatch, ...) and currently requires at least a
// non-empty code; error lines are rendered as "ERROR <code> <message>".
// The official code table is not frozen yet, so names other than the three
// used by the public pack are best guesses.
func errorCode(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "duplicate primary key"):
		return "duplicate_key"
	case strings.Contains(m, "dimension mismatch"):
		return "dimension_mismatch"
	case strings.Contains(m, "invalid vector"),
		strings.Contains(m, "finite float32"),
		strings.Contains(m, "float32 distance computation overflow"),
		strings.Contains(m, "invalid distance arguments"):
		return "invalid_vector"
	case strings.Contains(m, "exceeded") && strings.Contains(m, "varchar"):
		return "value_too_long"
	case strings.Contains(m, "requires an integer"),
		strings.Contains(m, "requires sql quotes"),
		strings.Contains(m, "invalid utf-8"),
		strings.Contains(m, "invalid sql string quote"),
		strings.Contains(m, "invalid finite number"):
		return "type_mismatch"
	case strings.Contains(m, "unknown table"):
		return "table_not_found"
	case strings.Contains(m, "table exists"):
		return "table_exists"
	case strings.Contains(m, "unknown") && strings.Contains(m, "column"):
		return "column_not_found"
	case strings.Contains(m, "index exists"):
		return "index_exists"
	case strings.Contains(m, "index requires vector column"),
		strings.Contains(m, "primary key requires bigint"),
		strings.Contains(m, "requires one bigint primary key"):
		return "invalid_schema"
	case strings.Contains(m, "transaction"):
		return "transaction_error"
	case strings.Contains(m, "unsupported"):
		return "unsupported_sql"
	case strings.Contains(m, "wal"), strings.Contains(m, "storage"),
		strings.Contains(m, "not ready"), strings.Contains(m, "snapshot"):
		return "internal_error"
	}
	return "sql_error"
}
