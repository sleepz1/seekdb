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

// Planted near-duplicates plus distractors: nearby vectors can cross sign
// boundaries, and 24 of 32 coordinates are invisible to the index signature.
func TestM11ANNRecallRepresentative(t *testing.T) {
	const dims, clusters, perCluster, distractors, k = 32, 32, 40, 256, 10
	const seed = 1101
	rng := rand.New(rand.NewSource(seed))
	centers := make([][]float32, clusters)
	for c := range centers {
		centers[c] = make([]float32, dims)
		for j := range centers[c] {
			centers[c][j] = float32(rng.NormFloat64())
		}
	}
	vectorSQL := func(v []float32) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	var vectors [][]float32
	var values []string
	add := func(v []float32) {
		id := len(vectors)
		vectors = append(vectors, v)
		values = append(values, fmt.Sprintf("(%d,%d,%s)", id, id%2, vectorSQL(v)))
	}
	for _, center := range centers {
		for i := 0; i < perCluster; i++ {
			v := make([]float32, dims)
			for j := range v {
				v[j] = center[j] + float32(.15*rng.NormFloat64())
			}
			add(v)
		}
	}
	for i := 0; i < distractors; i++ {
		v := make([]float32, dims)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
		}
		add(v)
	}
	s := openTest(t, t.TempDir()).Session()
	execTest(t, s, "CREATE TABLE t (id BIGINT PRIMARY KEY, category BIGINT, v VECTOR(32))")
	execTest(t, s, "INSERT INTO t VALUES "+strings.Join(values, ","))
	execTest(t, s, "CREATE VECTOR INDEX ix ON t(v)")
	queries := make([][]float32, clusters)
	for c, center := range centers {
		q := make([]float32, dims)
		for j := range q {
			q[j] = center[j] + float32(.08*rng.NormFloat64())
		}
		queries[c] = q
	}
	for _, filtered := range []bool{false, true} {
		name := "unfiltered"
		if filtered {
			name = "filtered"
		}
		t.Run(name, func(t *testing.T) {
			total, minimum, crossing := 0.0, 1.0, 0
			var exactTime, annTime time.Duration
			for qi, q := range queries {
				// Independent float64 L2 accumulation over stored float32 inputs,
				// rounded to float32 with ascending primary-key ties. This fixture
				// has enough separation to agree with engine float32 accumulation.
				type neighbor struct {
					id    int
					score float32
				}
				var truth []neighbor
				for id, v := range vectors {
					if filtered && id%2 != 1 {
						continue
					}
					sum := 0.0
					for j, x := range v {
						d := float64(x) - float64(q[j])
						sum += d * d
					}
					truth = append(truth, neighbor{id, float32(math.Sqrt(sum))})
				}
				sort.Slice(truth, func(i, j int) bool {
					if truth[i].score == truth[j].score {
						return truth[i].id < truth[j].id
					}
					return truth[i].score < truth[j].score
				})
				want := map[int64]bool{}
				for _, n := range truth[:k] {
					want[int64(n.id)] = true
					if bucket(vectors[n.id]) != bucket(q) {
						crossing++
					}
				}
				where := ""
				if filtered {
					where = " WHERE category = 1"
				}
				sql := "SELECT id FROM t" + where + " ORDER BY L2_DISTANCE(v," + vectorSQL(q) + ") LIMIT 10"
				execTest(t, s, "SET search_mode=exact")
				start := time.Now()
				exact := execTest(t, s, sql)
				exactTime += time.Since(start)
				if len(exact.Rows) != k {
					t.Fatal("exact cardinality", exact)
				}
				for i, r := range exact.Rows {
					if r[0] != int64(truth[i].id) {
						t.Fatalf("query=%d exact rank=%d got=%v want=%d", qi, i, r, truth[i].id)
					}
				}
				execTest(t, s, "SET search_mode=ann")
				start = time.Now()
				ann := execTest(t, s, sql)
				annTime += time.Since(start)
				if len(ann.Rows) != k {
					t.Fatal("ANN cardinality", ann)
				}
				seen := map[int64]bool{}
				hits := 0
				for _, r := range ann.Rows {
					id := r[0].(int64)
					if id < 0 || id >= int64(len(vectors)) || seen[id] || (filtered && id%2 != 1) {
						t.Fatal("invalid/duplicate/unfiltered ANN ID", id)
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
			mean := total / float64(len(queries))
			t.Logf("dims=%d seed=%d rows=%d queries=%d mean recall@10=%.5f min=%.2f cross-sign exact neighbors=%d/320", dims, seed, len(vectors), len(queries), mean, minimum, crossing)
			t.Logf("M12 timing: 32 SQL Top10 queries exact=%.3f ms ann=%.3f ms (mode switches and oracle excluded)", float64(exactTime)/float64(time.Millisecond), float64(annTime)/float64(time.Millisecond))
			if crossing == 0 {
				t.Fatal("fixture lost sign-boundary difficulty")
			}
			// M12 measured 1.00/1.00 in both groups on this unchanged fixture.
			// M17 locks this fixture at 1.00; not a hidden-distribution guarantee.
			if mean != 1 || minimum != 1 {
				t.Fatal("ANN recall degeneration")
			}
		})
	}

	// Force shell expansion after filtering: the only eligible row has a
	// different signature from a query with an occupied unfiltered bucket.
	execTest(t, s, "SET search_mode=ann")
	target := -1
	for id, v := range vectors {
		if bucket(v) != bucket(queries[0]) {
			target = id
			break
		}
	}
	if target < 0 {
		t.Fatal("missing cross-shell filter fixture")
	}
	sql := fmt.Sprintf("SELECT id FROM t WHERE id = %d ORDER BY L2_DISTANCE(v,%s) LIMIT 10", target, vectorSQL(queries[0]))
	r := execTest(t, s, sql)
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(target) {
		t.Fatal("filter must precede shell selection and TopK", r)
	}
	r = execTest(t, s, "SELECT id FROM t WHERE category = 9 ORDER BY L2_DISTANCE(v,"+vectorSQL(queries[0])+") LIMIT 10")
	if len(r.Rows) != 0 {
		t.Fatal("empty eligible set", r)
	}
}
