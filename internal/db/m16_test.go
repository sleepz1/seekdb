package db

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

// Larger than M13/M14; real byte-triggered automatic snapshot plus nonempty redo.
func TestM16RSSRecoveryScale(t *testing.T) {
	const rows, dims, batchRows = 16384, 32, 256
	for _, budget := range []int64{0, 4096} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			dir := t.TempDir()
			opts := OpenOptions{MemoryBudgetBytes: budget}
			e, err := OpenWithOptions(dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if e != nil {
					e.Close()
				}
			}()
			m9RSS(t, "M16 before workload")
			s := e.Session()
			execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, payload TEXT, v VECTOR(32))")
			payload := strings.Repeat("x", 640)
			for base := 0; base < rows; base += batchRows {
				values := make([]string, batchRows)
				for j := range values {
					id := base + j
					v := make([]string, dims)
					for d := range v {
						v[d] = fmt.Sprint((id+d)%97 - 48)
					}
					values[j] = fmt.Sprintf("(%d,'%s',[%s])", id, payload, strings.Join(v, ","))
				}
				execTest(t, s, "INSERT INTO t VALUES "+strings.Join(values, ","))
			}
			execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
			execTest(t, s, "UPDATE t SET payload = 'redo' WHERE id = 0")
			if e.snapshotLSN == 0 || e.version <= e.snapshotLSN || m14WALSize(t, e) == 0 {
				t.Fatal("missing automatic snapshot + redo")
			}
			lsn, version, walBytes := e.snapshotLSN, e.version, m14WALSize(t, e)
			m9RSS(t, "M16 after 16384 rows x 32D + auto-checkpoint")
			execTest(t, s, "BEGIN")
			execTest(t, s, "DELETE FROM t")
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 2; pass++ {
				start := time.Now()
				e, err = OpenWithOptions(dir, opts)
				elapsed := time.Since(start)
				if err != nil {
					t.Fatal(err)
				}
				if e.version != version || e.snapshotLSN != lsn {
					t.Fatal("recovered LSN mismatch")
				}
				table := e.state["t"]
				if table.rowCount() != rows || len(table.Indexes) != 1 {
					t.Fatal("rows/index lost")
				}
				for id := 0; id < rows; id++ {
					row := table.rowAt(id)
					want := payload
					if id == 0 {
						want = "redo"
					}
					if row[0] != int64(id) || row[1] != want {
						t.Fatalf("row %d mismatch", id)
					}
					v := row[2].([]float32)
					if len(v) != dims {
						t.Fatal("dimension")
					}
					for d, x := range v {
						if x != float32((id+d)%97-48) {
							t.Fatal("vector mismatch", id, d)
						}
					}
				}
				seen := make(map[int]bool)
				for key, offsets := range table.Indexes[0].buckets {
					for _, n := range offsets {
						if n < 0 || n >= rows || seen[n] || bucket(table.rowAt(n)[2].([]float32)) != key {
							t.Fatal("invalid bucket", n)
						}
						seen[n] = true
					}
				}
				if len(seen) != rows {
					t.Fatal("incomplete index")
				}
				s = e.Session()
				q := strings.TrimSuffix(strings.Repeat("0,", dims), ",")
				for _, mode := range []string{"exact", "ann"} {
					execTest(t, s, "SET search_mode="+mode)
					r := execTest(t, s, "SELECT id FROM t WHERE id = 12345 ORDER BY L2_DISTANCE(v,["+q+"]) LIMIT 10")
					if len(r.Rows) != 1 || r.Rows[0][0] != int64(12345) {
						t.Fatal(mode, r)
					}
				}
				t.Logf("M16 recovery: budget=%d rows=%d dims=%d snapshot_lsn=%d version=%d wal_bytes=%d pass=%d duration=%s rows/sec=%.0f", budget, rows, dims, lsn, version, walBytes, pass, elapsed, float64(rows)/elapsed.Seconds())
				m9RSS(t, "M16 after recovery + exact/ANN")
				t.Logf("M16 peak decoded cache=%d bytes; budget=%d is not RSS", e.PeakResidentRowBytes(), budget)
				if budget > 0 && e.PeakResidentRowBytes() > budget {
					t.Fatal("cache budget")
				}
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Independent isotropic Gaussian rows/queries, no planted near-duplicates.
func TestM16ANNGeneralization(t *testing.T) {
	testGaussian64(t, false)
}

// Shared unchanged M16 data generation and independent exact oracle.
func testGaussian64(t *testing.T, strict bool) {
	testGaussianANN(t, strict, 2048, 64, 32, 1601, false)
}

func testGaussianANN(t *testing.T, strict bool, rows, dims, queries int, seed int64, rss bool) {
	const k = 10
	if rss {
		m9RSS(t, "M18 before build")
	}
	rng := rand.New(rand.NewSource(seed))
	vector := func() []float32 {
		v := make([]float32, dims)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
		}
		return v
	}
	sqlVector := func(v []float32) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	vectors := make([][]float32, rows)
	values := make([]string, rows)
	for id := range vectors {
		vectors[id] = vector()
		values[id] = fmt.Sprintf("(%d,%d,%s)", id, id%3, sqlVector(vectors[id]))
	}
	e := openTest(t, t.TempDir())
	s := e.Session()
	execTest(t, s, fmt.Sprintf("CREATE TABLE t (id BIGINT PRIMARY KEY, category BIGINT, v VECTOR(%d))", dims))
	execTest(t, s, "INSERT INTO t VALUES "+strings.Join(values, ","))
	execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
	if rss {
		m9RSS(t, "M18 after build")
		defer m9RSS(t, "M18 after queries")
	}
	qs := make([][]float32, queries)
	for i := range qs {
		qs[i] = vector()
	}
	for _, filtered := range []bool{false, true} {
		t.Run(fmt.Sprint(filtered), func(t *testing.T) {
			total, minimum := 0.0, 1.0
			candidateMin, candidateMax := rows, 0
			for _, q := range qs {
				type neighbor struct {
					id    int
					score float32
				}
				truth := []neighbor{}
				for id, v := range vectors {
					if filtered && id%3 != 1 {
						continue
					}
					// Match documented float32 accumulation independently of distance().
					var sum float32
					for d, x := range v {
						delta := x - q[d]
						sum += delta * delta
					}
					truth = append(truth, neighbor{id, float32(math.Sqrt(float64(sum)))})
				}
				sort.Slice(truth, func(i, j int) bool {
					if truth[i].score == truth[j].score {
						return truth[i].id < truth[j].id
					}
					return truth[i].score < truth[j].score
				})
				where := ""
				if filtered {
					where = " WHERE category = 1"
				}
				sql := "SELECT id FROM t" + where + " ORDER BY L2_DISTANCE(v," + sqlVector(q) + ") LIMIT 10"
				execTest(t, s, "SET search_mode=exact")
				exact := execTest(t, s, sql)
				if len(exact.Rows) != k {
					t.Fatal("exact cardinality")
				}
				want := map[int64]bool{}
				for i, r := range exact.Rows {
					if r[0] != int64(truth[i].id) {
						t.Fatal("exact oracle mismatch")
					}
					want[r[0].(int64)] = true
				}
				if strict {
					table := e.state["t"]
					preds, err := parseWhere(table, strings.TrimPrefix(where, " WHERE "))
					if err != nil {
						t.Fatal(err)
					}
					candidates := indexCandidateOffsets(table, &table.Indexes[0], q, preds, k)
					// Bound distance-scored candidates relative to eligible rows.
					// This catches exact fallback, including after filtering.
					if len(candidates) < k || len(candidates)*4 > len(truth)*3 {
						t.Fatalf("ANN candidates=%d eligible=%d: expected K through 75%%", len(candidates), len(truth))
					}
					seenOffsets := map[int]bool{}
					for _, n := range candidates {
						if n < 0 || n >= rows || seenOffsets[n] || !matches(table.rowAt(n), preds) {
							t.Fatal("invalid, duplicate or ineligible candidate", n)
						}
						seenOffsets[n] = true
					}
					if len(candidates) < candidateMin {
						candidateMin = len(candidates)
					}
					if len(candidates) > candidateMax {
						candidateMax = len(candidates)
					}
				}
				execTest(t, s, "SET search_mode=ann")
				ann := execTest(t, s, sql)
				if len(ann.Rows) != k {
					t.Fatal("ANN cardinality")
				}
				hits := 0
				seen := map[int64]bool{}
				for _, r := range ann.Rows {
					id := r[0].(int64)
					if id < 0 || id >= int64(rows) || seen[id] || (filtered && id%3 != 1) {
						t.Fatal("invalid ANN ID")
					}
					seen[id] = true
					if want[id] {
						hits++
					}
				}
				recall := float64(hits) / k
				total += recall
				if recall < minimum {
					minimum = recall
				}
			}
			mean := total / float64(queries)
			t.Logf("ANN Gaussian seed=%d dims=%d rows=%d queries=%d filtered=%t recall@10 mean=%.5f min=%.2f", seed, dims, rows, queries, filtered, mean, minimum)
			if strict {
				t.Logf("Gaussian%d filtered=%t mean=%.5f min=%.2f candidates min/max=%d/%d", dims, filtered, mean, minimum, candidateMin, candidateMax)
				// M17 measured .84062/.60 and .81250/.50; M18 measured
				// .79063/.50 and .77500/.50. Floors allow modest headroom.
				if mean < .75 || minimum < .40 {
					t.Fatal("Gaussian recall regression")
				}
			}
			// Original M16 floor retained alongside the stronger M17 regression.
			// Per-query minimum is diagnostic: no universal recall guarantee.
			if mean < .20 {
				t.Fatal("Gaussian mean recall regression")
			}
		})
	}
}
