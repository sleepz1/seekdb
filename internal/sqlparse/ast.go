package sqlparse

// ColType is a column data type.
type ColType int

const (
	TypeBigint ColType = iota + 1
	TypeVarchar
	TypeVector
)

func (t ColType) String() string {
	switch t {
	case TypeBigint:
		return "BIGINT"
	case TypeVarchar:
		return "VARCHAR"
	case TypeVector:
		return "VECTOR"
	}
	return "?"
}

// Stmt is any parsed statement.
type Stmt interface{ stmt() }

type ColumnDef struct {
	Name    string
	Type    ColType
	Param   int // VARCHAR length or VECTOR dimension; 0 = unbounded VARCHAR
	Primary bool
}

type CreateTable struct {
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	PrimaryKey  string // column name, resolved from inline or table constraint
}

type DropTable struct {
	Name     string
	IfExists bool
}

type CreateIndex struct {
	Name        string
	Table       string
	Column      string
	IfNotExists bool
}

type DropIndex struct {
	Name  string
	Table string
}

type Insert struct {
	Table   string
	Columns []string // nil = all columns in table order
	Rows    [][]Expr
}

type SelectItem struct {
	Star  bool
	Expr  Expr
	Alias string // empty if none
	Text  string // original text, used for the column label when no alias
}

type OrderItem struct {
	Expr Expr
	Desc bool
}

type Select struct {
	Items  []SelectItem
	Table  string // empty for SELECT without FROM
	Where  Expr
	Order  []OrderItem
	Limit  int64 // -1 = no limit
	Offset int64
	Approx bool // APPROX / APPROXIMATE keyword present
}

type Assignment struct {
	Column string
	Value  Expr
}

type Update struct {
	Table string
	Sets  []Assignment
	Where Expr
}

type Delete struct {
	Table string
	Where Expr
}

type Begin struct{}
type Commit struct{}
type Rollback struct{}

type Set struct {
	Name  string
	Value string // lower-cased literal or identifier text
}

type ShowTables struct{}

type Exit struct{}

func (*CreateTable) stmt() {}
func (*DropTable) stmt()   {}
func (*CreateIndex) stmt() {}
func (*DropIndex) stmt()   {}
func (*Insert) stmt()      {}
func (*Select) stmt()      {}
func (*Update) stmt()      {}
func (*Delete) stmt()      {}
func (*Begin) stmt()       {}
func (*Commit) stmt()      {}
func (*Rollback) stmt()    {}
func (*Set) stmt()         {}
func (*ShowTables) stmt()  {}
func (*Exit) stmt()        {}

// Expr is an expression node.
type Expr interface{ expr() }

type LitKind int

const (
	LitInt LitKind = iota + 1
	LitFloat
	LitString
	LitVector
	LitNull
)

type Literal struct {
	Kind LitKind
	Text string   // numeric text (with sign) or string value
	Vec  []string // element texts for bracket vector literals
}

type ColRef struct{ Name string }

type Func struct {
	Name string // upper-cased
	Args []Expr
}

type Binary struct {
	Op   string // = != < <= > >= AND OR
	L, R Expr
}

type Not struct{ X Expr }

func (*Literal) expr() {}
func (*ColRef) expr()  {}
func (*Func) expr()    {}
func (*Binary) expr()  {}
func (*Not) expr()     {}
