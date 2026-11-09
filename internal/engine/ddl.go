package engine

import (
	"strings"

	"minivectordb/internal/sqlparse"
)

func (s *Session) execDDL(st sqlparse.Stmt) (*Result, error) {
	e := s.e
	switch x := st.(type) {
	case *sqlparse.CreateTable:
		key := strings.ToLower(x.Name)
		if e.tables[key] != nil {
			if x.IfNotExists {
				return &Result{}, nil
			}
			return nil, newErr(ErrTableExists, "Table '%s' already exists", x.Name)
		}
		sch, err := buildSchema(x)
		if err != nil {
			return nil, err
		}
		p := &payloadBuf{}
		encodeSchema(p, sch)
		if err := e.commitDDL(p.b); err != nil {
			return nil, err
		}
		e.tables[key] = newTable(sch)
		return &Result{}, nil

	case *sqlparse.DropTable:
		key := strings.ToLower(x.Name)
		t := e.tables[key]
		if t == nil {
			if x.IfExists {
				return &Result{}, nil
			}
			return nil, newErr(ErrNoTable, "Unknown table '%s'", x.Name)
		}
		p := &payloadBuf{}
		p.byte(opDropTable)
		p.str(t.schema.Name)
		if err := e.commitDDL(p.b); err != nil {
			return nil, err
		}
		e.dropCache(t)
		delete(e.tables, key)
		return &Result{}, nil

	case *sqlparse.CreateIndex:
		t, err := e.table(x.Table)
		if err != nil {
			return nil, err
		}
		ci := t.schema.colIndex(x.Column)
		if ci < 0 {
			return nil, newErr(ErrNoColumn, "Key column '%s' doesn't exist in table", x.Column)
		}
		if t.schema.Columns[ci].Type != sqlparse.TypeVector {
			return nil, newErr(ErrBadIndexColumn, "vector index requires a VECTOR column, '%s' is %s", x.Column, t.schema.Columns[ci].Type)
		}
		for _, other := range e.tables {
			if other.vecIndex != "" && strings.EqualFold(other.vecIndex, x.Name) {
				if x.IfNotExists && other == t {
					return &Result{}, nil
				}
				return nil, newErr(ErrIndexExists, "Duplicate key name '%s'", x.Name)
			}
		}
		if t.vecIndex != "" {
			if x.IfNotExists {
				return &Result{}, nil
			}
			return nil, newErr(ErrIndexExists, "table '%s' already has vector index '%s'", t.schema.Name, t.vecIndex)
		}
		p := &payloadBuf{}
		p.byte(opCreateIndex)
		p.str(x.Name)
		p.str(t.schema.Name)
		if err := e.commitDDL(p.b); err != nil {
			return nil, err
		}
		t.vecIndex = x.Name
		return &Result{}, nil

	case *sqlparse.DropIndex:
		t, err := e.table(x.Table)
		if err != nil {
			return nil, err
		}
		if t.vecIndex == "" || !strings.EqualFold(t.vecIndex, x.Name) {
			return nil, newErr(ErrNoIndex, "Can't DROP '%s'; check that column/key exists", x.Name)
		}
		p := &payloadBuf{}
		p.byte(opDropIndex)
		p.str(t.vecIndex)
		p.str(t.schema.Name)
		if err := e.commitDDL(p.b); err != nil {
			return nil, err
		}
		t.vecIndex = ""
		return &Result{}, nil
	}
	return nil, newErr(ErrUnsupported, "unsupported statement")
}

func buildSchema(x *sqlparse.CreateTable) (*Schema, error) {
	if len(x.Columns) == 0 {
		return nil, newErr(ErrParse, "a table must have at least one column")
	}
	sch := &Schema{Name: x.Name, PK: -1, VecCol: -1}
	seen := map[string]bool{}
	for i, c := range x.Columns {
		k := strings.ToLower(c.Name)
		if seen[k] {
			return nil, newErr(ErrDupColumn, "Duplicate column name '%s'", c.Name)
		}
		seen[k] = true
		if c.Type == sqlparse.TypeVector {
			if sch.VecCol >= 0 {
				return nil, newErr(ErrMultiVector, "a table may have at most one VECTOR column")
			}
			sch.VecCol = i
		}
		sch.Columns = append(sch.Columns, Column{Name: c.Name, Type: c.Type, Param: c.Param})
	}
	if x.PrimaryKey == "" {
		return nil, newErr(ErrNoPK, "a BIGINT PRIMARY KEY is required")
	}
	pk := sch.colIndex(x.PrimaryKey)
	if pk < 0 {
		return nil, newErr(ErrNoColumn, "Key column '%s' doesn't exist in table", x.PrimaryKey)
	}
	if sch.Columns[pk].Type != sqlparse.TypeBigint {
		return nil, newErr(ErrBadPKType, "the primary key must be a BIGINT column")
	}
	sch.PK = pk
	return sch, nil
}
