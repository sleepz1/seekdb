package engine

import (
	"errors"
	"fmt"

	"minivectordb/internal/sqlparse"
)

// ErrKind classifies statement errors. Each kind maps to a MySQL-compatible
// error number and SQLSTATE so the front end can render whatever error form
// the test driver expects.
type ErrKind int

const (
	ErrParse ErrKind = iota + 1
	ErrUnsupported
	ErrNoTable
	ErrTableExists
	ErrNoColumn
	ErrDupColumn
	ErrDupKey
	ErrNoPK
	ErrMultiPK
	ErrBadPKType
	ErrMultiVector
	ErrDataTooLong
	ErrTypeMismatch
	ErrVectorDim
	ErrVectorValue
	ErrValueCount
	ErrNullValue
	ErrOutOfRange
	ErrPKUpdate
	ErrIndexExists
	ErrNoIndex
	ErrBadIndexColumn
	ErrTxnState
	ErrInternal
)

// SQLError is an error with a stable classification.
type SQLError struct {
	Kind ErrKind
	Msg  string
}

func (e *SQLError) Error() string { return e.Msg }

// Code returns the MySQL error number.
func (e *SQLError) Code() int {
	switch e.Kind {
	case ErrParse:
		return 1064
	case ErrUnsupported:
		return 1235
	case ErrNoTable:
		return 1146
	case ErrTableExists:
		return 1050
	case ErrNoColumn:
		return 1054
	case ErrDupColumn:
		return 1060
	case ErrDupKey:
		return 1062
	case ErrNoPK:
		return 1173
	case ErrMultiPK:
		return 1068
	case ErrBadPKType, ErrMultiVector, ErrBadIndexColumn:
		return 1221
	case ErrDataTooLong:
		return 1406
	case ErrTypeMismatch:
		return 1366
	case ErrVectorDim, ErrVectorValue:
		return 1366
	case ErrValueCount:
		return 1136
	case ErrNullValue:
		return 1048
	case ErrOutOfRange:
		return 1264
	case ErrPKUpdate:
		return 1235
	case ErrIndexExists:
		return 1061
	case ErrNoIndex:
		return 1091
	case ErrTxnState:
		return 1399
	}
	return 1105
}

// Name returns the semantic error name used by the public test pack
// (e.g. duplicate_key, unsupported_sql, dimension_mismatch).
func (e *SQLError) Name() string {
	switch e.Kind {
	case ErrDupKey:
		return "duplicate_key"
	case ErrUnsupported, ErrPKUpdate:
		return "unsupported_sql"
	case ErrVectorDim:
		return "dimension_mismatch"
	case ErrVectorValue:
		return "invalid_vector"
	case ErrNoTable:
		return "table_not_found"
	case ErrTableExists:
		return "table_exists"
	case ErrNoColumn:
		return "column_not_found"
	case ErrDataTooLong:
		return "value_too_long"
	case ErrTypeMismatch:
		return "type_mismatch"
	case ErrValueCount:
		return "value_count_mismatch"
	case ErrNullValue:
		return "null_not_allowed"
	case ErrOutOfRange:
		return "out_of_range"
	case ErrIndexExists:
		return "index_exists"
	case ErrNoIndex:
		return "index_not_found"
	case ErrNoPK, ErrMultiPK, ErrBadPKType, ErrMultiVector, ErrBadIndexColumn, ErrDupColumn:
		return "invalid_schema"
	case ErrTxnState:
		return "transaction_error"
	case ErrParse:
		return "syntax_error"
	}
	return "internal_error"
}

// SQLState returns the five character SQLSTATE.
func (e *SQLError) SQLState() string {
	switch e.Kind {
	case ErrParse, ErrNoTable, ErrTableExists, ErrNoColumn, ErrDupColumn, ErrMultiPK, ErrIndexExists, ErrNoIndex:
		return "42000"
	case ErrDupKey, ErrNullValue:
		return "23000"
	case ErrDataTooLong, ErrOutOfRange:
		return "22001"
	case ErrTypeMismatch, ErrVectorDim, ErrVectorValue:
		return "HY000"
	case ErrValueCount:
		return "21S01"
	}
	return "HY000"
}

func newErr(k ErrKind, format string, a ...any) *SQLError {
	return &SQLError{Kind: k, Msg: fmt.Sprintf(format, a...)}
}

// AsSQLError converts any error to a *SQLError.
func AsSQLError(err error) *SQLError {
	var se *SQLError
	if errors.As(err, &se) {
		return se
	}
	var syn *sqlparse.SyntaxError
	if errors.As(err, &syn) {
		return &SQLError{Kind: ErrParse, Msg: syn.Msg}
	}
	var un *sqlparse.UnsupportedError
	if errors.As(err, &un) {
		return &SQLError{Kind: ErrUnsupported, Msg: un.Msg}
	}
	var sem *sqlparse.SemanticError
	if errors.As(err, &sem) {
		if sem.Code == "multiple_pk" {
			return &SQLError{Kind: ErrMultiPK, Msg: sem.Msg}
		}
		return &SQLError{Kind: ErrParse, Msg: sem.Msg}
	}
	return &SQLError{Kind: ErrInternal, Msg: err.Error()}
}
