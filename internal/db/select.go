package db

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Find a keyword only outside SQL strings and nested expressions.
func keyword(s, word string) int {
	quoted, depth := false, 0
	upper := strings.ToUpper(s)
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			if quoted && i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			quoted = !quoted
			continue
		}
		if quoted {
			continue
		}
		switch s[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		}
		if depth == 0 && strings.HasPrefix(upper[i:], word) {
			return i
		}
	}
	return -1
}

type expression struct {
	index  int
	metric string
	query  []float32
	name   string
}

var metricRE = regexp.MustCompile(`(?is)^(L2_DISTANCE|DISTANCE|COSINE|DOT|EUCLIDEAN)\s*\((.*)\)$`)
var aliasRE = regexp.MustCompile(`(?is)^(.*?)\s+AS\s+(\w+)$`)
var implicitAliasRE = regexp.MustCompile(`(?is)^(.*\))\s+(\w+)$`)
var directionRE = regexp.MustCompile(`(?is)^(.*?)\s+(ASC|DESC)$`)

func parseExpression(t *Table, s string) (expression, error) {
	e := expression{index: -1, name: s}
	if m := metricRE.FindStringSubmatch(s); m != nil {
		args, err := split(m[2])
		if err != nil || len(args) != 2 {
			return e, fmt.Errorf("invalid distance arguments")
		}
		e.index = columnIndex(t, args[0])
		if e.index < 0 || t.Columns[e.index].Type != "VECTOR" {
			return e, fmt.Errorf("unknown vector column")
		}
		e.metric = strings.ToUpper(m[1])
		e.query, err = parseVector(args[1], t.Columns[e.index].Dim)
		return e, err
	}
	e.index = columnIndex(t, s)
	if e.index < 0 {
		return e, fmt.Errorf("unknown column %s", s)
	}
	e.name = t.Columns[e.index].Name
	return e, nil
}
func (e expression) eval(row []any) (any, error) {
	if e.metric == "" {
		return row[e.index], nil
	}
	score := distance(e.metric, row[e.index].([]float32), e.query)
	if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
		return nil, fmt.Errorf("float32 distance computation overflow")
	}
	return score, nil
}
func selectRows(state State, sql string, ann bool) (Result, error) {
	fail := func(s string) (Result, error) { return Result{}, fmt.Errorf("%s", s) }
	pos := keyword(sql, " FROM ")
	if pos < 0 {
		return fail("expected FROM")
	}
	projection := strings.TrimSpace(sql[len("SELECT "):pos])
	rest := strings.TrimSpace(sql[pos+6:])
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fail("missing table")
	}
	t := state[strings.ToLower(fields[0])]
	if t == nil {
		return fail("unknown table")
	}
	rest = strings.TrimSpace(rest[len(fields[0]):])
	clauses := map[string]string{}
	// Prefix a space so keyword detection also handles the first clause.
	rest = " " + rest
	last := 0
	for _, key := range []string{" WHERE ", " ORDER BY ", " LIMIT "} {
		p := keyword(rest, key)
		if p < 0 {
			continue
		}
		if p < last {
			return fail("invalid clause order")
		}
		last = p
		if keyword(rest[p+len(key):], key) >= 0 {
			return fail("duplicate SELECT clause")
		}
		end := len(rest)
		for _, other := range []string{" WHERE ", " ORDER BY ", " LIMIT "} {
			if q := keyword(rest[p+len(key):], other); q >= 0 && p+len(key)+q < end {
				end = p + len(key) + q
			}
		}
		clauses[key] = strings.TrimSpace(rest[p+len(key) : end])
		if clauses[key] == "" {
			return fail("empty clause")
		}
	}
	if strings.TrimSpace(rest) != "" {
		valid := false
		for _, key := range []string{" WHERE ", " ORDER BY ", " LIMIT "} {
			if strings.HasPrefix(rest, key) {
				valid = true
			}
		}
		if !valid {
			return fail("unsupported SELECT suffix")
		}
	}
	preds, err := parseWhere(t, clauses[" WHERE "])
	if err != nil {
		return Result{}, err
	}
	exprs := []expression{}
	if projection == "*" {
		for i, c := range t.Columns {
			exprs = append(exprs, expression{index: i, name: c.Name})
		}
	} else {
		parts, err := split(projection)
		if err != nil {
			return Result{}, err
		}
		for _, part := range parts {
			name := ""
			if m := aliasRE.FindStringSubmatch(part); m != nil {
				part = strings.TrimSpace(m[1])
				name = m[2]
			} else if m := implicitAliasRE.FindStringSubmatch(part); m != nil {
				part = strings.TrimSpace(m[1])
				name = m[2]
			}
			e, err := parseExpression(t, part)
			if err != nil {
				return Result{}, err
			}
			if name != "" {
				e.name = name
			}
			exprs = append(exprs, e)
		}
	}
	order := clauses[" ORDER BY "]
	desc := false
	var ordering expression
	if order != "" {
		if m := directionRE.FindStringSubmatch(order); m != nil {
			order = strings.TrimSpace(m[1])
			desc = strings.EqualFold(m[2], "DESC")
		}
		found := false
		for _, e := range exprs {
			if strings.EqualFold(order, e.name) {
				if found {
					return fail("ambiguous ORDER BY alias")
				}
				ordering = e
				found = true
			}
		}
		if !found {
			ordering, err = parseExpression(t, order)
			if err != nil {
				return Result{}, err
			}
		}
		if ordering.metric == "" && t.Columns[ordering.index].Type == "VECTOR" {
			return fail("cannot order vector column")
		}
	}
	rows := [][]any{}
	for _, row := range t.Rows {
		if matches(row, preds) {
			rows = append(rows, row)
		}
	}
	if ann && order != "" && ordering.metric != "" {
		if idx := vectorIndex(t, ordering.index); idx != nil {
			rows = indexCandidates(t, idx, ordering.query, preds)
		} else {
			rows = annCandidates(rows)
		}
	}
	type item struct {
		row []any
		key any
	}
	items := make([]item, len(rows))
	for i, row := range rows {
		items[i].row = row
		if order != "" {
			items[i].key, err = ordering.eval(row)
			if err != nil {
				return Result{}, err
			}
		}
	}
	pk := primaryIndex(t)
	if order != "" {
		sort.SliceStable(items, func(i, j int) bool {
			c := compare(items[i].key, items[j].key)
			if c == 0 {
				return pk >= 0 && compare(items[i].row[pk], items[j].row[pk]) < 0
			}
			if desc {
				return c > 0
			}
			return c < 0
		})
	}
	if lim := clauses[" LIMIT "]; lim != "" {
		n, err := strconv.Atoi(lim)
		if err != nil || n < 0 {
			return fail("invalid LIMIT")
		}
		if n < len(items) {
			items = items[:n]
		}
	}
	r := Result{}
	for _, e := range exprs {
		r.Columns = append(r.Columns, e.name)
	}
	for _, it := range items {
		row := []any{}
		for _, e := range exprs {
			v, err := e.eval(it.row)
			if err != nil {
				return Result{}, err
			}
			row = append(row, v)
		}
		r.Rows = append(r.Rows, row)
	}
	return r, nil
}
