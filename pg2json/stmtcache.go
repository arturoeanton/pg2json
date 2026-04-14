package pg2json

import (
	"hash/fnv"
	"strconv"
	"sync"

	"github.com/arturoeanton/pg2json/internal/rows"
)

// preparedStmt is the per-(SQL, param-types) state cached on a Client.
//   - name: the server-side statement name we used in Parse.
//   - plan: the compiled row decoder built from the first RowDescription.
//   - resultFmts: the format codes we asked for in Bind (so we can rebuild
//     the plan if the description changes — defensive).
type preparedStmt struct {
	name       string
	plan       *rows.Plan
	resultFmts []int16
}

// stmtCache is a tiny LRU-ish cache keyed by SQL string. We don't bother
// keying on parameter OIDs because pg2json always sends parameters in text
// format and lets the server infer; if you Prepare two queries with the
// same SQL but cast their args differently, they'll share a plan, which
// is what you want.
//
// Capacity is bounded; eviction is "drop-anything" (a real LRU adds memory
// for marginal benefit at cache sizes <256). Eviction sends a Close
// message to the server; we batch that with the next query for free.
type stmtCache struct {
	mu     sync.Mutex
	max    int
	byKey  map[string]*preparedStmt
	closed []string // server-side names pending Close
	seq    uint64
}

func newStmtCache(max int) *stmtCache {
	if max <= 0 {
		max = 64
	}
	return &stmtCache{max: max, byKey: make(map[string]*preparedStmt, max)}
}

func (sc *stmtCache) get(sql string) *preparedStmt {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.byKey[sql]
}

func (sc *stmtCache) put(sql string, st *preparedStmt) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.byKey) >= sc.max {
		// Evict any one entry. Iterating a map gives us a random key,
		// which is fine for this purpose.
		for k, v := range sc.byKey {
			sc.closed = append(sc.closed, v.name)
			delete(sc.byKey, k)
			break
		}
	}
	sc.byKey[sql] = st
}

func (sc *stmtCache) takePendingClose() []string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.closed) == 0 {
		return nil
	}
	out := sc.closed
	sc.closed = nil
	return out
}

// nextName generates a unique server-side statement name. Short-by-design:
// statement names are sent on the wire on every Bind.
func (sc *stmtCache) nextName() string {
	sc.mu.Lock()
	sc.seq++
	n := sc.seq
	sc.mu.Unlock()
	var buf [24]byte
	dst := append(buf[:0], 's', '_')
	dst = strconv.AppendUint(dst, n, 36)
	return string(dst)
}

// invalidate drops sql from the cache (e.g. after a query failure that
// might have left server state inconsistent). The name is queued for Close.
func (sc *stmtCache) invalidate(sql string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if st, ok := sc.byKey[sql]; ok {
		sc.closed = append(sc.closed, st.name)
		delete(sc.byKey, sql)
	}
}

// hashKey is exposed for diagnostic use; we key on the raw SQL string.
func hashKey(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}
