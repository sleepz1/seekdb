// Command minivectordb runs the database in the foreground. SQL arrives on
// stdin, results go to stdout, logs go to stderr. No network listener.
package main

import (
	"bufio"
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
	format := fs.String("format", envOr("MVDB_FORMAT", "json"), "output format: json or tsv")
	ready := fs.String("ready-line", os.Getenv("MVDB_READY_LINE"), "line printed to stdout once recovery finishes (empty = none)")
	cacheMB := fs.Int64("cache-mb", 0, "row cache budget in MiB (0 = derive from cgroup limit)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *mode != "exact" && *mode != "ann" {
		fmt.Fprintf(stderr, "minivectordb: invalid --mode %q\n", *mode)
		return 2
	}
	var out formatter
	switch *format {
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

// execOne runs a statement and writes its response. It reports whether the
// statement asked the process to exit.
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
// quotes and comments. Until the first ';' has been seen, a newline outside
// quotes with balanced brackets also ends a statement (drivers that send one
// unterminated statement per line). After a ';' has been seen the driver is
// known to terminate statements, so newlines are plain whitespace and
// multi-line statements work.
type splitter struct {
	r    *bufio.Reader
	eof  bool
	semi bool // a ';' terminator has been observed
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
			s.semi = true
			if t := strings.TrimSpace(sb.String()); t != "" {
				return t, true, nil
			}
			sb.Reset()
			depth = 0
		case '\n':
			if depth == 0 && !s.semi {
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
