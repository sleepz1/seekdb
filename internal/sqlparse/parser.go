package sqlparse

import (
	"strconv"
	"strings"
)

type parser struct {
	src  string
	toks []token
	i    int
}

// Parse parses exactly one statement. A trailing semicolon is allowed.
func Parse(src string) (Stmt, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	st, err := p.statement()
	if err != nil {
		return nil, err
	}
	for p.isSym(";") {
		p.i++
	}
	if p.peek().kind != tEOF {
		return nil, p.errNear()
	}
	return st, nil
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) peekAt(k int) token {
	if p.i+k < len(p.toks) {
		return p.toks[p.i+k]
	}
	return p.toks[len(p.toks)-1]
}
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) errNear() error {
	t := p.peek()
	if t.kind == tEOF {
		return syntaxErr("syntax error: unexpected end of statement")
	}
	return syntaxErr("syntax error near '%s'", near(p.src, t.pos))
}

func (p *parser) isKw(kw string) bool {
	t := p.peek()
	return t.kind == tIdent && t.upper() == kw
}

func (p *parser) acceptKw(kw string) bool {
	if p.isKw(kw) {
		p.i++
		return true
	}
	return false
}

func (p *parser) expectKw(kw string) error {
	if !p.acceptKw(kw) {
		return p.errNear()
	}
	return nil
}

func (p *parser) isSym(s string) bool {
	t := p.peek()
	return t.kind == tSymbol && t.text == s
}

func (p *parser) acceptSym(s string) bool {
	if p.isSym(s) {
		p.i++
		return true
	}
	return false
}

func (p *parser) expectSym(s string) error {
	if !p.acceptSym(s) {
		return p.errNear()
	}
	return nil
}

var reserved = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "ORDER": true, "BY": true, "LIMIT": true,
	"INSERT": true, "INTO": true, "VALUES": true, "UPDATE": true, "SET": true, "DELETE": true,
	"CREATE": true, "TABLE": true, "DROP": true, "AND": true, "OR": true, "NOT": true,
	"ASC": true, "DESC": true, "AS": true, "OFFSET": true, "PRIMARY": true, "KEY": true,
	"INDEX": true, "ON": true, "APPROX": true, "APPROXIMATE": true,
}

// ident reads an identifier, optionally qualified (db.table keeps the last part).
func (p *parser) ident() (string, error) {
	t := p.peek()
	switch t.kind {
	case tQuotedIdent:
		p.i++
		return t.text, nil
	case tIdent:
		if reserved[t.upper()] {
			return "", p.errNear()
		}
		p.i++
		return t.text, nil
	}
	return "", p.errNear()
}

func (p *parser) qualifiedIdent() (string, error) {
	name, err := p.ident()
	if err != nil {
		return "", err
	}
	for p.isSym(".") {
		p.i++
		name, err = p.ident()
		if err != nil {
			return "", err
		}
	}
	return name, nil
}

func (p *parser) statement() (Stmt, error) {
	t := p.peek()
	if t.kind != tIdent {
		return nil, p.errNear()
	}
	switch t.upper() {
	case "CREATE":
		p.i++
		return p.create()
	case "DROP":
		p.i++
		return p.drop()
	case "INSERT":
		p.i++
		return p.insert()
	case "SELECT":
		p.i++
		return p.selectStmt()
	case "UPDATE":
		p.i++
		return p.update()
	case "DELETE":
		p.i++
		return p.deleteStmt()
	case "BEGIN":
		p.i++
		p.acceptKw("WORK")
		return &Begin{}, nil
	case "START":
		p.i++
		if err := p.expectKw("TRANSACTION"); err != nil {
			return nil, err
		}
		return &Begin{}, nil
	case "COMMIT":
		p.i++
		p.acceptKw("WORK")
		return &Commit{}, nil
	case "ROLLBACK":
		p.i++
		p.acceptKw("WORK")
		return &Rollback{}, nil
	case "SET":
		p.i++
		return p.set()
	case "SHOW":
		p.i++
		if err := p.expectKw("TABLES"); err != nil {
			return nil, err
		}
		return &ShowTables{}, nil
	case "EXIT", "QUIT":
		p.i++
		return &Exit{}, nil
	case "ALTER", "TRUNCATE", "RENAME", "REPLACE", "GRANT", "REVOKE", "LOCK", "UNLOCK",
		"MERGE", "CALL", "EXPLAIN", "DESCRIBE", "DESC", "USE", "WITH", "SAVEPOINT", "RELEASE":
		return nil, &UnsupportedError{Msg: t.upper() + " is not supported"}
	}
	return nil, p.errNear()
}

func (p *parser) create() (Stmt, error) {
	if p.acceptKw("TABLE") {
		return p.createTable()
	}
	if p.acceptKw("VECTOR") {
		if err := p.expectKw("INDEX"); err != nil {
			return nil, err
		}
		return p.createIndex()
	}
	if p.acceptKw("INDEX") {
		return p.createIndex()
	}
	return nil, p.errNear()
}

func (p *parser) ifNotExists() (bool, error) {
	if p.acceptKw("IF") {
		if err := p.expectKw("NOT"); err != nil {
			return false, err
		}
		if err := p.expectKw("EXISTS"); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (p *parser) createTable() (Stmt, error) {
	ine, err := p.ifNotExists()
	if err != nil {
		return nil, err
	}
	name, err := p.qualifiedIdent()
	if err != nil {
		return nil, err
	}
	ct := &CreateTable{Name: name, IfNotExists: ine}
	if err := p.expectSym("("); err != nil {
		return nil, err
	}
	for {
		if p.isKw("PRIMARY") {
			p.i++
			if err := p.expectKw("KEY"); err != nil {
				return nil, err
			}
			if err := p.expectSym("("); err != nil {
				return nil, err
			}
			col, err := p.ident()
			if err != nil {
				return nil, err
			}
			if p.isSym(",") {
				return nil, &UnsupportedError{Msg: "composite primary keys are not supported"}
			}
			if err := p.expectSym(")"); err != nil {
				return nil, err
			}
			if ct.PrimaryKey != "" {
				return nil, &SemanticError{Code: "multiple_pk", Msg: "Multiple primary key defined"}
			}
			ct.PrimaryKey = col
		} else {
			cd, err := p.columnDef()
			if err != nil {
				return nil, err
			}
			if cd.Primary {
				if ct.PrimaryKey != "" {
					return nil, &SemanticError{Code: "multiple_pk", Msg: "Multiple primary key defined"}
				}
				ct.PrimaryKey = cd.Name
			}
			ct.Columns = append(ct.Columns, cd)
		}
		if p.acceptSym(",") {
			continue
		}
		if err := p.expectSym(")"); err != nil {
			return nil, err
		}
		break
	}
	// Ignore common table options such as ENGINE=... or CHARSET=...
	for p.peek().kind == tIdent {
		p.i++
		p.acceptSym("=")
		if p.peek().kind == tIdent || p.peek().kind == tString || p.peek().kind == tNumber {
			p.i++
		} else {
			return nil, p.errNear()
		}
	}
	return ct, nil
}

func (p *parser) positiveInt() (int, error) {
	t := p.peek()
	if t.kind != tNumber {
		return 0, p.errNear()
	}
	v, err := strconv.Atoi(t.text)
	if err != nil {
		return 0, p.errNear()
	}
	p.i++
	return v, nil
}

func (p *parser) columnDef() (ColumnDef, error) {
	name, err := p.ident()
	if err != nil {
		return ColumnDef{}, err
	}
	cd := ColumnDef{Name: name}
	t := p.peek()
	if t.kind != tIdent {
		return cd, p.errNear()
	}
	switch t.upper() {
	case "BIGINT", "INT", "INTEGER":
		p.i++
		cd.Type = TypeBigint
		if p.acceptSym("(") { // display width, ignored
			if _, err := p.positiveInt(); err != nil {
				return cd, err
			}
			if err := p.expectSym(")"); err != nil {
				return cd, err
			}
		}
	case "VARCHAR", "CHAR":
		p.i++
		cd.Type = TypeVarchar
		if err := p.expectSym("("); err != nil {
			return cd, err
		}
		n, err := p.positiveInt()
		if err != nil {
			return cd, err
		}
		if n <= 0 {
			return cd, &SemanticError{Code: "bad_type", Msg: "VARCHAR length must be positive"}
		}
		cd.Param = n
		if err := p.expectSym(")"); err != nil {
			return cd, err
		}
	case "VECTOR":
		p.i++
		cd.Type = TypeVector
		if err := p.expectSym("("); err != nil {
			return cd, err
		}
		n, err := p.positiveInt()
		if err != nil {
			return cd, err
		}
		if n <= 0 {
			return cd, &SemanticError{Code: "bad_type", Msg: "VECTOR dimension must be positive"}
		}
		cd.Param = n
		if err := p.expectSym(")"); err != nil {
			return cd, err
		}
	default:
		return cd, &UnsupportedError{Msg: "unsupported column type " + t.text}
	}
	for {
		switch {
		case p.isKw("PRIMARY"):
			p.i++
			if err := p.expectKw("KEY"); err != nil {
				return cd, err
			}
			cd.Primary = true
		case p.isKw("NOT"):
			p.i++
			if err := p.expectKw("NULL"); err != nil {
				return cd, err
			}
		case p.isKw("NULL"):
			return cd, &UnsupportedError{Msg: "NULL columns are not supported"}
		case p.isKw("DEFAULT"), p.isKw("AUTO_INCREMENT"), p.isKw("UNIQUE"):
			return cd, &UnsupportedError{Msg: "unsupported column attribute " + p.peek().text}
		default:
			return cd, nil
		}
	}
}

func (p *parser) createIndex() (Stmt, error) {
	ine, err := p.ifNotExists()
	if err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("ON"); err != nil {
		return nil, err
	}
	table, err := p.qualifiedIdent()
	if err != nil {
		return nil, err
	}
	if err := p.expectSym("("); err != nil {
		return nil, err
	}
	col, err := p.ident()
	if err != nil {
		return nil, err
	}
	if p.isSym(",") {
		return nil, &UnsupportedError{Msg: "multi-column vector indexes are not supported"}
	}
	if err := p.expectSym(")"); err != nil {
		return nil, err
	}
	// Optional index options, e.g. WITH (distance=l2, type=hnsw); accepted and recorded nowhere.
	if p.acceptKw("WITH") {
		if err := p.expectSym("("); err != nil {
			return nil, err
		}
		depth := 1
		for depth > 0 {
			t := p.next()
			if t.kind == tEOF {
				return nil, p.errNear()
			}
			if t.kind == tSymbol && t.text == "(" {
				depth++
			} else if t.kind == tSymbol && t.text == ")" {
				depth--
			}
		}
	}
	return &CreateIndex{Name: name, Table: table, Column: col, IfNotExists: ine}, nil
}

func (p *parser) drop() (Stmt, error) {
	if p.acceptKw("TABLE") {
		ie := false
		if p.acceptKw("IF") {
			if err := p.expectKw("EXISTS"); err != nil {
				return nil, err
			}
			ie = true
		}
		name, err := p.qualifiedIdent()
		if err != nil {
			return nil, err
		}
		if p.isSym(",") {
			return nil, &UnsupportedError{Msg: "dropping several tables at once is not supported"}
		}
		return &DropTable{Name: name, IfExists: ie}, nil
	}
	p.acceptKw("VECTOR")
	if p.acceptKw("INDEX") {
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("ON"); err != nil {
			return nil, err
		}
		table, err := p.qualifiedIdent()
		if err != nil {
			return nil, err
		}
		return &DropIndex{Name: name, Table: table}, nil
	}
	return nil, p.errNear()
}

func (p *parser) insert() (Stmt, error) {
	if err := p.expectKw("INTO"); err != nil {
		return nil, err
	}
	table, err := p.qualifiedIdent()
	if err != nil {
		return nil, err
	}
	ins := &Insert{Table: table}
	if p.acceptSym("(") {
		for {
			c, err := p.ident()
			if err != nil {
				return nil, err
			}
			ins.Columns = append(ins.Columns, c)
			if p.acceptSym(",") {
				continue
			}
			if err := p.expectSym(")"); err != nil {
				return nil, err
			}
			break
		}
	}
	if !p.acceptKw("VALUES") && !p.acceptKw("VALUE") {
		return nil, p.errNear()
	}
	for {
		if err := p.expectSym("("); err != nil {
			return nil, err
		}
		var row []Expr
		if !p.isSym(")") {
			for {
				e, err := p.expr()
				if err != nil {
					return nil, err
				}
				row = append(row, e)
				if p.acceptSym(",") {
					continue
				}
				break
			}
		}
		if err := p.expectSym(")"); err != nil {
			return nil, err
		}
		ins.Rows = append(ins.Rows, row)
		if p.acceptSym(",") {
			continue
		}
		break
	}
	return ins, nil
}

func (p *parser) selectStmt() (Stmt, error) {
	sel := &Select{Limit: -1}
	for {
		if p.isSym("*") {
			p.i++
			sel.Items = append(sel.Items, SelectItem{Star: true, Text: "*"})
		} else {
			start := p.peek().pos
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			end := p.peek().pos
			item := SelectItem{Expr: e, Text: strings.TrimSpace(p.src[start:end])}
			if p.acceptKw("AS") {
				a, err := p.aliasName()
				if err != nil {
					return nil, err
				}
				item.Alias = a
			} else if t := p.peek(); t.kind == tQuotedIdent || t.kind == tString || (t.kind == tIdent && !reserved[t.upper()]) {
				a, err := p.aliasName()
				if err != nil {
					return nil, err
				}
				item.Alias = a
			}
			sel.Items = append(sel.Items, item)
		}
		if p.acceptSym(",") {
			continue
		}
		break
	}
	if p.acceptKw("FROM") {
		t, err := p.qualifiedIdent()
		if err != nil {
			return nil, err
		}
		sel.Table = t
		if p.acceptKw("AS") {
			if _, err := p.ident(); err != nil {
				return nil, err
			}
		}
		if p.isSym(",") || p.isKw("JOIN") || p.isKw("INNER") || p.isKw("LEFT") || p.isKw("RIGHT") || p.isKw("CROSS") {
			return nil, &UnsupportedError{Msg: "JOIN is not supported"}
		}
	}
	if p.acceptKw("WHERE") {
		w, err := p.expr()
		if err != nil {
			return nil, err
		}
		sel.Where = w
	}
	if p.isKw("GROUP") || p.isKw("HAVING") {
		return nil, &UnsupportedError{Msg: "GROUP BY / HAVING is not supported"}
	}
	if p.acceptKw("ORDER") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			oi := OrderItem{Expr: e}
			if p.acceptKw("DESC") {
				oi.Desc = true
			} else {
				p.acceptKw("ASC")
			}
			sel.Order = append(sel.Order, oi)
			if p.acceptSym(",") {
				continue
			}
			break
		}
		if p.acceptKw("APPROX") || p.acceptKw("APPROXIMATE") {
			sel.Approx = true
		}
	}
	if p.acceptKw("LIMIT") {
		a, err := p.nonNegInt()
		if err != nil {
			return nil, err
		}
		if p.acceptSym(",") {
			b, err := p.nonNegInt()
			if err != nil {
				return nil, err
			}
			sel.Offset, sel.Limit = a, b
		} else {
			sel.Limit = a
			if p.acceptKw("OFFSET") {
				b, err := p.nonNegInt()
				if err != nil {
					return nil, err
				}
				sel.Offset = b
			}
		}
	}
	if p.isKw("FOR") || p.isKw("UNION") {
		return nil, &UnsupportedError{Msg: "unsupported clause " + p.peek().text}
	}
	return sel, nil
}

func (p *parser) aliasName() (string, error) {
	t := p.peek()
	switch t.kind {
	case tQuotedIdent, tString:
		p.i++
		return t.text, nil
	case tIdent:
		if reserved[t.upper()] {
			return "", p.errNear()
		}
		p.i++
		return t.text, nil
	}
	return "", p.errNear()
}

func (p *parser) nonNegInt() (int64, error) {
	t := p.peek()
	if t.kind != tNumber {
		return 0, p.errNear()
	}
	v, err := strconv.ParseInt(t.text, 10, 64)
	if err != nil || v < 0 {
		return 0, p.errNear()
	}
	p.i++
	return v, nil
}

func (p *parser) update() (Stmt, error) {
	table, err := p.qualifiedIdent()
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("SET"); err != nil {
		return nil, err
	}
	up := &Update{Table: table}
	for {
		c, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectSym("="); err != nil {
			return nil, err
		}
		e, err := p.expr()
		if err != nil {
			return nil, err
		}
		up.Sets = append(up.Sets, Assignment{Column: c, Value: e})
		if p.acceptSym(",") {
			continue
		}
		break
	}
	if p.acceptKw("WHERE") {
		w, err := p.expr()
		if err != nil {
			return nil, err
		}
		up.Where = w
	}
	if p.isKw("ORDER") || p.isKw("LIMIT") {
		return nil, &UnsupportedError{Msg: "UPDATE with ORDER BY/LIMIT is not supported"}
	}
	return up, nil
}

func (p *parser) deleteStmt() (Stmt, error) {
	if err := p.expectKw("FROM"); err != nil {
		return nil, err
	}
	table, err := p.qualifiedIdent()
	if err != nil {
		return nil, err
	}
	d := &Delete{Table: table}
	if p.acceptKw("WHERE") {
		w, err := p.expr()
		if err != nil {
			return nil, err
		}
		d.Where = w
	}
	if p.isKw("ORDER") || p.isKw("LIMIT") {
		return nil, &UnsupportedError{Msg: "DELETE with ORDER BY/LIMIT is not supported"}
	}
	return d, nil
}

func (p *parser) set() (Stmt, error) {
	p.acceptKw("SESSION")
	p.acceptKw("GLOBAL")
	if p.isSym("@") {
		p.i++
	}
	if p.isKw("NAMES") {
		p.i++
		p.next() // charset
		if p.acceptKw("COLLATE") {
			p.next()
		}
		return &Set{Name: "names"}, nil
	}
	t := p.peek()
	if t.kind != tIdent && t.kind != tQuotedIdent {
		return nil, p.errNear()
	}
	p.i++
	name := strings.ToLower(t.text)
	for p.acceptSym(".") { // e.g. @@session.var
		t = p.next()
		name = strings.ToLower(t.text)
	}
	if !p.acceptSym("=") && !p.acceptKw("TO") {
		return nil, p.errNear()
	}
	v := p.next()
	switch v.kind {
	case tIdent, tString, tNumber, tQuotedIdent:
	default:
		return nil, p.errNear()
	}
	return &Set{Name: name, Value: strings.ToLower(v.text)}, nil
}

// Expression grammar: or -> and -> not -> comparison -> primary.
func (p *parser) expr() (Expr, error) { return p.orExpr() }

func (p *parser) orExpr() (Expr, error) {
	l, err := p.andExpr()
	if err != nil {
		return nil, err
	}
	for p.isKw("OR") {
		p.i++
		r, err := p.andExpr()
		if err != nil {
			return nil, err
		}
		l = &Binary{Op: "OR", L: l, R: r}
	}
	return l, nil
}

func (p *parser) andExpr() (Expr, error) {
	l, err := p.notExpr()
	if err != nil {
		return nil, err
	}
	for p.isKw("AND") {
		p.i++
		r, err := p.notExpr()
		if err != nil {
			return nil, err
		}
		l = &Binary{Op: "AND", L: l, R: r}
	}
	return l, nil
}

func (p *parser) notExpr() (Expr, error) {
	if p.isKw("NOT") {
		p.i++
		x, err := p.notExpr()
		if err != nil {
			return nil, err
		}
		return &Not{X: x}, nil
	}
	return p.cmpExpr()
}

func (p *parser) cmpExpr() (Expr, error) {
	l, err := p.primary()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind == tSymbol {
		switch t.text {
		case "=", "!=", "<>", "<", "<=", ">", ">=":
			p.i++
			r, err := p.primary()
			if err != nil {
				return nil, err
			}
			op := t.text
			if op == "<>" {
				op = "!="
			}
			return &Binary{Op: op, L: l, R: r}, nil
		}
	}
	if t.kind == tIdent {
		switch t.upper() {
		case "IS", "IN", "LIKE", "BETWEEN", "REGEXP":
			return nil, &UnsupportedError{Msg: "operator " + t.upper() + " is not supported"}
		}
	}
	return l, nil
}

func (p *parser) primary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tNumber:
		p.i++
		return numLit(t.text), nil
	case tString:
		p.i++
		return &Literal{Kind: LitString, Text: t.text}, nil
	case tQuotedIdent:
		p.i++
		return &ColRef{Name: t.text}, nil
	case tSymbol:
		switch t.text {
		case "-", "+":
			p.i++
			nt := p.peek()
			if nt.kind != tNumber {
				return nil, p.errNear()
			}
			p.i++
			if t.text == "-" {
				return numLit("-" + nt.text), nil
			}
			return numLit(nt.text), nil
		case "(":
			p.i++
			e, err := p.expr()
			if err != nil {
				return nil, err
			}
			if err := p.expectSym(")"); err != nil {
				return nil, err
			}
			return e, nil
		case "[":
			return p.vectorLit()
		}
		return nil, p.errNear()
	case tIdent:
		up := t.upper()
		if up == "NULL" {
			p.i++
			return &Literal{Kind: LitNull}, nil
		}
		if up == "TRUE" || up == "FALSE" {
			return nil, &UnsupportedError{Msg: "boolean literals are not supported"}
		}
		if p.peekAt(1).kind == tSymbol && p.peekAt(1).text == "(" {
			p.i += 2
			f := &Func{Name: up}
			if !p.isSym(")") {
				for {
					a, err := p.expr()
					if err != nil {
						return nil, err
					}
					f.Args = append(f.Args, a)
					if p.acceptSym(",") {
						continue
					}
					break
				}
			}
			if err := p.expectSym(")"); err != nil {
				return nil, err
			}
			return f, nil
		}
		if reserved[up] {
			return nil, p.errNear()
		}
		p.i++
		name := t.text
		for p.isSym(".") { // table.column -> column
			p.i++
			n, err := p.ident()
			if err != nil {
				return nil, err
			}
			name = n
		}
		return &ColRef{Name: name}, nil
	}
	return nil, p.errNear()
}

func (p *parser) vectorLit() (Expr, error) {
	if err := p.expectSym("["); err != nil {
		return nil, err
	}
	lit := &Literal{Kind: LitVector}
	if p.acceptSym("]") {
		return lit, nil
	}
	for {
		sign := ""
		if p.isSym("-") || p.isSym("+") {
			sign = p.next().text
		}
		t := p.peek()
		if t.kind == tNumber {
			p.i++
			lit.Vec = append(lit.Vec, sign+t.text)
		} else if t.kind == tIdent {
			// NaN / Inf etc. are carried through so the engine can reject them.
			p.i++
			lit.Vec = append(lit.Vec, sign+t.text)
		} else {
			return nil, p.errNear()
		}
		if p.acceptSym(",") {
			continue
		}
		break
	}
	if err := p.expectSym("]"); err != nil {
		return nil, err
	}
	return lit, nil
}

func numLit(text string) *Literal {
	if strings.ContainsAny(text, ".eE") {
		return &Literal{Kind: LitFloat, Text: text}
	}
	return &Literal{Kind: LitInt, Text: text}
}

// UnsupportedError marks syntactically recognised but unsupported features.
type UnsupportedError struct{ Msg string }

func (e *UnsupportedError) Error() string { return e.Msg }

// SemanticError marks invalid definitions detected while parsing.
type SemanticError struct {
	Code string
	Msg  string
}

func (e *SemanticError) Error() string { return e.Msg }
