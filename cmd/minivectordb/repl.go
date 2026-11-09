package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"minivectordb/internal/db"
	"strings"
)

// resultCodec selects stdout framing. Default interimTSV matches tip#1 / current
// public-sample runners. Official codec is 待题包确认 — do not flip default
// without driver evidence (see FORMAL-MYSQL-GAP-MATRIX.md).
type resultCodec int

const (
	codecInterimTSV resultCodec = iota
)

// activeResultCodec is a test hook; production stays at codecInterimTSV.
var activeResultCodec = codecInterimTSV

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
		// Test-driver directives such as "--restart" and "--mode ann" are
		// not SQL and take no assertion slot; if a driver forwards them on
		// stdin they must produce no output, or every later response would
		// be shifted by one line. Results stay exact, which also satisfies
		// the ann expectations.
		if strings.HasPrefix(statement, "--") {
			return nil
		}
		if err := writeResult(w, session.Execute(statement)); err != nil {
			return err
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

func writeResult(w *bufio.Writer, result db.Result) error {
	switch activeResultCodec {
	default: // codecInterimTSV
		if result.Error != "" {
			fmt.Fprintln(w, "ERROR "+errorCode(result.Error)+" "+escape(result.Error))
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
	}
	return nil
}

func escape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n", "\r", "\\r").Replace(s)
}
