// Command minivectordb runs the database in the foreground.
// The default mode speaks the official stdin/stdout JSONL protocol
// (STDIO_PROTOCOL.md): one JSON object per line, a ready handshake,
// and logs only on stderr. No network listener.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"minivectordb/internal/engine"
	"minivectordb/internal/sqlparse"
	"minivectordb/internal/xfmt"
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
	opts := cmdOptions{
		dataDir: envOr("MVDB_DATA_DIR", "/data"),
		mode:    envOr("MVDB_SEARCH_MODE", "exact"),
		format:  envOr("MVDB_FORMAT", "jsonl"),
		ready:   os.Getenv("MVDB_READY_LINE"),
	}
	if err := parseFlags(args, &opts); err != nil {
		io.WriteString(stderr, err.Error()+"\n"+usage)
		return 2
	}
	dataDir, mode, format, ready, cacheMB := &opts.dataDir, &opts.mode, &opts.format, &opts.ready, &opts.cacheMB
	if *mode != "exact" && *mode != "ann" {
		io.WriteString(stderr, xfmt.Sprintf("minivectordb: invalid --mode %q\n", *mode))
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
		io.WriteString(stderr, xfmt.Sprintf("minivectordb: invalid --format %q\n", *format))
		return 2
	}

	engine.ConfigureCPU()
	eng, err := engine.Open(engine.Options{Dir: *dataDir, CacheBudget: *cacheMB << 20})
	if err != nil {
		io.WriteString(stderr, "minivectordb: startup failed: "+err.Error()+"\n")
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
		io.WriteString(stderr, "minivectordb: "+s.String()+" received, shutting down\n")
		shutdown(0)
	}()

	io.WriteString(stderr, "minivectordb: ready\n")
	if protocolMode {
		if _, err := w.WriteString("{\"type\":\"ready\"}\n"); err != nil {
			io.WriteString(stderr, "minivectordb: output error: "+err.Error()+"\n")
			return 1
		}
		if err := w.Flush(); err != nil {
			io.WriteString(stderr, "minivectordb: output error: "+err.Error()+"\n")
			return 1
		}
		return serveJSONL(sess, eng, stdin, w, stderr, &mu)
	}

	if *ready != "" {
		w.WriteString(*ready + "\n")
		if err := w.Flush(); err != nil {
			return 1
		}
	}

	sc := newSplitter(stdin)
	for {
		stmt, ok, err := sc.next()
		if err != nil {
			io.WriteString(stderr, "minivectordb: input error: "+err.Error()+"\n")
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
			io.WriteString(stderr, "minivectordb: output error: "+ferr.Error()+"\n")
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
	ID         []byte // raw JSON value of "id"
	Op         string
	SQL        string
	SearchMode string

	fast bool // decoded by fastDecode: id is a scalar copied verbatim
}

// serveJSONL implements the official stdin/stdout protocol. Ready has already
// been written and flushed. On shutdown it returns the process exit code.
func serveJSONL(sess *engine.Session, eng *engine.Engine, stdin io.Reader, w *bufio.Writer, stderr io.Writer, mu *sync.Mutex) int {
	br := bufio.NewReaderSize(stdin, 1<<20)
	for {
		// The line is only read during this iteration (the decoder copies
		// what it keeps), so a view into the reader's buffer is enough;
		// lines longer than the buffer are assembled on the heap.
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long := append([]byte(nil), line...)
			var rest []byte
			rest, err = br.ReadBytes('\n')
			line = append(long, rest...)
		}
		if err != nil && err != io.EOF {
			io.WriteString(stderr, "minivectordb: input error: "+err.Error()+"\n")
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
			io.WriteString(stderr, "minivectordb: output error: "+ferr.Error()+"\n")
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
	if err := decodeRequest(line, &req); err != nil {
		io.WriteString(stderr, "minivectordb: invalid JSON request: "+err.Error()+"\n")
		writeProtoError(w, []byte("null"), "invalid_request", err.Error())
		return false, 0
	}
	id := req.ID
	if len(bytes.TrimSpace(id)) == 0 || string(id) == "null" {
		// Still echo whatever arrived; missing id becomes null.
		if len(bytes.TrimSpace(id)) == 0 {
			id = []byte("null")
		}
	}
	switch req.Op {
	case "shutdown":
		sess.Close()
		if err := eng.Close(); err != nil {
			io.WriteString(stderr, "minivectordb: close error: "+err.Error()+"\n")
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
		if req.fast {
			fastWriteResult(w, id, res)
		} else {
			writeProtoResult(w, id, res)
		}
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

type cmdOptions struct {
	dataDir, mode, format, ready string
	cacheMB                      int64
}

const usage = `Usage of minivectordb:
  -cache-mb int
    	row cache budget in MiB (0 = derive from cgroup limit)
  -data string
    	alias of --data-dir
  -data-dir string
    	data directory (default "/data")
  -format string
    	output format: jsonl (official protocol, default), tsv, or json (legacy SQL stdin)
  -mode string
    	default vector search mode: exact or ann
  -ready-line string
    	legacy SQL mode only: line printed to stdout after recovery
`

// parseFlags reads the options with the syntax of package flag: -name value,
// -name=value, one or two leading dashes, stopping at the first non-flag
// argument or after "--".
func parseFlags(args []string, o *cmdOptions) error {
	for len(args) > 0 {
		a := args[0]
		if len(a) < 2 || a[0] != '-' {
			return nil
		}
		args = args[1:]
		name := a[1:]
		if name[0] == '-' {
			name = name[1:]
			if name == "" { // "--" ends the flags
				return nil
			}
		}
		if name == "" || name[0] == '-' || name[0] == '=' {
			return errors.New("bad flag syntax: " + a)
		}
		value, hasValue := "", false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, value, hasValue = name[:eq], name[eq+1:], true
		}
		var dst *string
		switch name {
		case "data-dir", "data":
			dst = &o.dataDir
		case "mode":
			dst = &o.mode
		case "format":
			dst = &o.format
		case "ready-line":
			dst = &o.ready
		case "cache-mb":
		case "h", "help":
			return errors.New("flag: help requested")
		default:
			return errors.New("flag provided but not defined: -" + name)
		}
		if !hasValue {
			if len(args) == 0 {
				return errors.New("flag needs an argument: -" + name)
			}
			value, args = args[0], args[1:]
		}
		if dst != nil {
			*dst = value
			continue
		}
		n, err := strconv.ParseInt(value, 0, 64)
		if err != nil {
			return errors.New("invalid value \"" + value + "\" for flag -" + name + ": parse error")
		}
		o.cacheMB = n
	}
	return nil
}
