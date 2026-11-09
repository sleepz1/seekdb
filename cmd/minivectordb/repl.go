package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"minivectordb/internal/db"
	"strings"
)

// Newlines terminate statements outside quotes and parentheses/brackets.
// Semicolons terminate statements outside quotes, including malformed SQL.
func repl(in io.Reader, out io.Writer, session *db.Session) error {
	defer session.Execute("ROLLBACK")
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)
	var sql strings.Builder
	quoted, depth := false, 0
	execute := func() error {
		statement := strings.TrimSpace(sql.String())
		sql.Reset()
		depth = 0
		if statement == "" {
			return nil
		}
		result := session.Execute(statement)
		if result.Error != "" {
			fmt.Fprintln(w, "ERROR "+escape(result.Error))
		} else if len(result.Columns) > 0 {
			fmt.Fprintln(w, strings.Join(result.Columns, "\t"))
			for _, row := range result.Rows {
				cells := make([]string, len(row))
				for i, value := range row {
					if s, ok := value.(string); ok {
						cells[i] = escape(s)
					} else {
						b, err := json.Marshal(value)
						if err != nil {
							return err
						}
						cells[i] = string(b)
					}
				}
				fmt.Fprintln(w, strings.Join(cells, "\t"))
			}
			fmt.Fprintf(w, "OK %d\n", len(result.Rows))
		} else {
			fmt.Fprintf(w, "OK %d\n", result.Affected)
		}
		return w.Flush()
	}
	for {
		c, err := r.ReadByte()
		if err == io.EOF {
			return execute()
		}
		if err != nil {
			return err
		}
		if c == '\'' {
			if quoted {
				next, _ := r.Peek(1)
				if len(next) == 1 && next[0] == '\'' {
					sql.WriteByte(c)
					c, _ = r.ReadByte()
					sql.WriteByte(c)
					continue
				}
			}
			quoted = !quoted
		}
		if !quoted {
			if c == ';' || (c == '\n' && depth == 0) {
				if err := execute(); err != nil {
					return err
				}
				continue
			}
			if c == '(' || c == '[' {
				depth++
			}
			if (c == ')' || c == ']') && depth > 0 {
				depth--
			}
		}
		sql.WriteByte(c)
	}
}

func escape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n", "\r", "\\r").Replace(s)
}
