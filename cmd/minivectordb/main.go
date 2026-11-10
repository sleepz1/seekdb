// Command minivectordb runs the database in the foreground.
// The default mode speaks the official stdin/stdout JSONL protocol
// (STDIO_PROTOCOL.md): one JSON object per line, a ready handshake,
// and logs only on stderr. No network listener.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"minivectordb/internal/engine"
	"minivectordb/internal/sqlparse"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("minivectordb", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", envOr("MVDB_DATA_DIR", "/data"), "data directory")
	fs.StringVar(dataDir, "data", *dataDir, "alias of --data-dir")
	mode := fs.String("mode", envOr("MVDB_SEARCH_MODE", "exact"), "default vector search mode: exact or ann")
	format := fs.String("format", envOr("MVDB_FORMAT", "jsonl"), "output format: jsonl (official protocol, default), tsv, or json (legacy SQL stdin)")
	ready := fs.String("ready-line", os.Getenv("MVDB_READY_LINE"), "legacy SQL mode only: line printed to stdout after recovery")
	cacheMB := fs.Int64("cache-mb", 0, "row cache budget in MiB (0 = derive from cgroup limit)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *mode != "exact" && *mode != "ann" {
		fmt.Fprintf(stderr, "minivectordb: invalid --mode %q\n", *mode)
		return 2
	}

	protocolMode := false
	var out formatter
	switch *format {
	case "jsonl":
		protocolMode = true
	case "json":
		out = jsonFormatter{}
	case "tsv":
		out = tsvFormatter{}
	default:
		fmt.Fprintf(stderr, "minivectordb: invalid --format %q\n", *format)
		return 2
	}

	engine.ConfigureCPU()
	eng, err := engine.Open(engine.Options{Dir: *dataDir, CacheBudget: *cacheMB << 20})
	if err != nil {
		fmt.Fprintf(stderr, "minivectordb: startup failed: %v\n", err)
		return 1
	}
	sess := eng.NewSession(*mode)
	w := bufio.NewWriterSize(stdout, 64<<10)

	var mu sync.Mutex // serialises statements with signal-driven shutdown
	shutdown := func(code int) {
		sess.Close() // rolls back an open transaction
		_ = w.Flush()
		_ = eng.Close()
		os.Exit(code)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		s := <-sigs
		mu.Lock()
		fmt.Fprintf(stderr, "minivectordb: %v received, shutting down\n", s)
		shutdown(0)
	}()

	fmt.Fprintln(stderr, "minivectordb: ready")
	if protocolMode {
		if _, err := w.WriteString("{\"type\":\"ready\"}\n"); err != nil {
			fmt.Fprintf(stderr, "minivectordb: output error: %v\n", err)
			return 1
		}
		if err := w.Flush(); err != nil {
			fmt.Fprintf(stderr, "minivectordb: output error: %v\n", err)
			return 1
		}
		return serveJSONL(sess, eng, stdin, w, stderr, &mu)
	}

	if *ready != "" {
		fmt.Fprintln(w, *ready)
		if err := w.Flush(); err != nil {
			return 1
		}
	}

	sc := newSplitter(stdin)
	for {
		stmt, ok, err := sc.next()
		if err != nil {
			fmt.Fprintf(stderr, "minivectordb: input error: %v\n", err)
			mu.Lock()
			shutdown(1)
		}
		if !ok {
			break
		}
		mu.Lock()
		exit := execOne(sess, stmt, out, w)
		ferr := w.Flush()
		if ferr != nil {
			fmt.Fprintf(stderr, "minivectordb: output error: %v\n", ferr)
			shutdown(1)
		}
		if exit {
			shutdown(0)
		}
		mu.Unlock()
	}
	mu.Lock()
	sess.Close()
	_ = w.Flush()
	_ = eng.Close()
	return 0
}

type protoRequest struct {
	ID         json.RawMessage `json:"id"`
	Op         string          `json:"op"`
	SQL        string          `json:"sql"`
	SearchMode string          `json:"search_mode"`
}

// serveJSONL implements the official stdin/stdout protocol. Ready has already
// been written and flushed. On shutdown it returns the process exit code.
func serveJSONL(sess *engine.Session, eng *engine.Engine, stdin io.Reader, w *bufio.Writer, stderr io.Writer, mu *sync.Mutex) int {
	br := bufio.NewReaderSize(stdin, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			fmt.Fprintf(stderr, "minivectordb: input error: %v\n", err)
			mu.Lock()
			sess.Close()
			_ = eng.Close()
			return 1
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			if err == io.EOF {
				mu.Lock()
				sess.Close()
				_ = eng.Close()
				return 0
			}
			continue
		}
		mu.Lock()
		exit, code := handleJSONL(sess, eng, line, w, stderr)
		if ferr := w.Flush(); ferr != nil {
			fmt.Fprintf(stderr, "minivectordb: output error: %v\n", ferr)
			sess.Close()
			_ = eng.Close()
			mu.Unlock()
			return 1
		}
		if exit {
			mu.Unlock()
			return code
		}
		mu.Unlock()
		if err == io.EOF {
			mu.Lock()
			sess.Close()
			_ = eng.Close()
			return 0
		}
	}
}

func handleJSONL(sess *engine.Session, eng *engine.Engine, line []byte, w *bufio.Writer, stderr io.Writer) (exit bool, code int) {
	var req protoRequest
	if err := json.Unmarshal(line, &req); err != nil {
		fmt.Fprintf(stderr, "minivectordb: invalid JSON request: %v\n", err)
		writeProtoError(w, json.RawMessage("null"), "invalid_request", err.Error())
		return false, 0
	}
	id := req.ID
	if len(bytes.TrimSpace(id)) == 0 || string(id) == "null" {
		// Still echo whatever arrived; missing id becomes null.
		if len(bytes.TrimSpace(id)) == 0 {
			id = json.RawMessage("null")
		}
	}
	switch req.Op {
	case "shutdown":
		sess.Close()
		if err := eng.Close(); err != nil {
			fmt.Fprintf(stderr, "minivectordb: close error: %v\n", err)
			writeProtoError(w, id, "internal_error", err.Error())
			return true, 1
		}
		writeProtoShutdown(w, id)
		return true, 0
	case "execute":
		if req.SearchMode == "ann" {
			sess.Mode = "ann"
		} else {
			sess.Mode = "exact"
		}
		res, err := sess.ExecSQL(req.SQL)
		if err != nil {
			se := engine.AsSQLError(err)
			writeProtoError(w, id, se.Name(), se.Msg)
			return false, 0
		}
		writeProtoResult(w, id, res)
		return false, 0
	default:
		writeProtoError(w, id, "unsupported_op", "unsupported operation")
		return false, 0
	}
}

// execOne runs a statement and writes its response. It reports whether the
// statement asked the process to exit. Used only by legacy SQL stdin modes.
func execOne(sess *engine.Session, sql string, out formatter, w *bufio.Writer) bool {
	st, err := sqlparse.Parse(sql)
	if err != nil {
		out.writeError(w, engine.AsSQLError(err))
		return false
	}
	if _, ok := st.(*sqlparse.Exit); ok {
		out.writeResult(w, &engine.Result{})
		return true
	}
	res, err := sess.Exec(st)
	if err != nil {
		out.writeError(w, engine.AsSQLError(err))
		return false
	}
	out.writeResult(w, res)
	return false
}

// splitter frames stdin into statements. A statement ends at ';' outside
// quotes and comments, or at a newline outside quotes when parentheses and
// brackets are balanced (one statement per line, with or without ';'). This
// matches the framing of the team's earlier submission, which the official
// driver accepted.
type splitter struct {
	r   *bufio.Reader
	eof bool
}

func newSplitter(r io.Reader) *splitter { return &splitter{r: bufio.NewReaderSize(r, 1<<20)} }

func (s *splitter) next() (string, bool, error) {
	var sb strings.Builder
	var quote byte
	depth := 0
	lineComment, blockComment := false, false
	for {
		if s.eof {
			break
		}
		c, err := s.r.ReadByte()
		if err == io.EOF {
			s.eof = true
			break
		}
		if err != nil {
			return "", false, err
		}
		if lineComment {
			if c == '\n' {
				lineComment = false
			} else {
				continue
			}
		}
		if blockComment {
			sb.WriteByte(c)
			if c == '/' {
				t := sb.String()
				if len(t) >= 2 && t[len(t)-2] == '*' {
					blockComment = false
				}
			}
			continue
		}
		if quote != 0 {
			sb.WriteByte(c)
			if c == '\\' && quote != '`' {
				if n, err := s.r.ReadByte(); err == nil {
					sb.WriteByte(n)
				}
				continue
			}
			if c == quote {
				if p, err := s.r.Peek(1); err == nil && p[0] == quote {
					b, _ := s.r.ReadByte()
					sb.WriteByte(b)
					continue
				}
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
			sb.WriteByte(c)
		case '(', '[':
			depth++
			sb.WriteByte(c)
		case ')', ']':
			if depth > 0 {
				depth--
			}
			sb.WriteByte(c)
		case ';':
			if t := strings.TrimSpace(sb.String()); t != "" {
				return t, true, nil
			}
			sb.Reset()
			depth = 0
		case '\n':
			if depth == 0 {
				if t := strings.TrimSpace(sb.String()); t != "" {
					return t, true, nil
				}
				sb.Reset()
				continue
			}
			sb.WriteByte(c)
		case '-':
			if p, err := s.r.Peek(1); err == nil && p[0] == '-' && strings.TrimSpace(sb.String()) == "" {
				lineComment = true
				continue
			}
			sb.WriteByte(c)
		case '/':
			sb.WriteByte(c)
			if p, err := s.r.Peek(1); err == nil && p[0] == '*' {
				b, _ := s.r.ReadByte()
				sb.WriteByte(b)
				blockComment = true
			}
		default:
			sb.WriteByte(c)
		}
	}
	if t := strings.TrimSpace(sb.String()); t != "" {
		return t, true, nil
	}
	return "", false, nil
}
