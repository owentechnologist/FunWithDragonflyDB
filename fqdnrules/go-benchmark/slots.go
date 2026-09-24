// Deterministic Redis-Cluster-slot-targeted bucket tokens: no Redis, no I/O.
//
// The __global__ and __tld__:<tld> tiers in router.go replicate across
// numBuckets physical keys so lookups spread across worker threads instead of
// hammering one key. This file computes, for a given numBuckets, N distinct
// opaque string tokens such that token i's CRC16 hash slot falls inside thread
// i's exclusive, equal-sized share of the 16384-slot Redis Cluster keyspace --
// a guarantee, not a coincidence of whatever slot a literal "<tag>:<i>" string
// happened to land in.
//
// The token table is a pure function of numBuckets alone (see
// ComputeBucketTable), shared by every namespace and every tag family, and is
// never persisted: every process recomputes the identical table. It is the Go
// port of fqdnrules/fqdn_slots.py and must stay semantically identical to it;
// slots_test.go pins both against the shared golden fixture.
//
// Layering: CRC16/slot/token-search logic lives here, not in pattern.go (which
// stays "no Redis, no I/O" and only decides which bucket a query belongs to)
// or in router.go (which only asks for a token and builds key strings with it).
package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Redis Cluster's fixed hash-slot space.
const numSlots = 16384

// Crc16 is CRC-16/XMODEM: poly 0x1021, init 0x0000, no reflection, no final
// XOR. This is the exact variant Redis Cluster's HASH_SLOT uses, easily
// confused with CRC-16/CCITT-FALSE, which differs only in init=0xFFFF.
// Known-answer vector: Crc16([]byte("123456789")) == 0x31C3.
func Crc16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// Slot is the 0..16383 Redis Cluster hash slot for raw bytes.
func Slot(data []byte) int {
	return int(Crc16(data)) & 0x3FFF
}

// KeySlot is the Redis-Cluster hash-tag-aware slot for a full key string. It
// hashes only the content of the first non-empty {...} span if one is present
// (matching Redis's own hash-tag extraction), otherwise the whole key.
func KeySlot(key string) int {
	if start := strings.Index(key, "{"); start != -1 {
		// offset 0 means the first "}" sits right after the "{": an empty span
		// is not a hash tag, and Redis does not look for a later "}" either.
		if offset := strings.Index(key[start+1:], "}"); offset > 0 {
			return Slot([]byte(key[start+1 : start+1+offset]))
		}
	}
	return Slot([]byte(key))
}

// SlotRange is bucket i's half-open share of the slot space, [lo, hi).
//
// Floor-boundary partition: boundaries[i] = floor(i * 16384 / N). Range widths
// differ by at most 1 slot; which buckets get the extra slot falls out of the
// floor arithmetic rather than an explicit remainder branch.
func SlotRange(i, numBuckets int) (lo, hi int) {
	return i * numSlots / numBuckets, (i + 1) * numSlots / numBuckets
}

// True iff [lo, hi) contains a slot s with s%numBuckets == bucketIndex. Go's %
// can go negative, so the residue is normalized before the comparison.
func bucketHasModuloMatch(lo, hi, numBuckets, bucketIndex int) bool {
	smallest := lo + ((bucketIndex-lo)%numBuckets+numBuckets)%numBuckets
	return smallest < hi
}

// True iff every bucket's contiguous range also contains a slot satisfying the
// interleaved slot%numBuckets == bucketIndex policy -- pure arithmetic,
// independent of CRC16. Empirically true for every 1 <= N <= 129 and for N in
// {16383, 16384} (each bucket's range there is so narrow, 1-2 slots, that it
// trivially contains its own modulo residue); false for every 130 <= N <= 16382.
func isModuloSafe(numBuckets int) bool {
	for i := 0; i < numBuckets; i++ {
		lo, hi := SlotRange(i, numBuckets)
		if !bucketHasModuloMatch(lo, hi, numBuckets, i) {
			return false
		}
	}
	return true
}

// Generous ceiling on candidate draws before ComputeBucketTable gives up
// loudly instead of spinning forever.
//
// Filling N bins by uniform random draws is coupon-collector: expected draws
// ~= N * ln(N). The modulo-safe search additionally requires
// slot%N == bucketIndex, roughly halving each draw's acceptance chance for its
// bucket, so this budgets a large constant-factor margin over the plain
// estimate rather than modelling the dual-constraint expectation exactly. 200x
// plus a flat floor comfortably covers both the modulo-safe and
// contiguous-only cases at every legal N, including N close to 16384. Same
// formula as fqdn_slots.py's _iteration_cap, so both sides fail at the same
// point if the search ever degenerates.
func iterationCap(numBuckets int) int {
	n := numBuckets
	if n < 2 {
		n = 2
	}
	return 200*numBuckets*int(math.Ceil(math.Log(float64(n)))) + 10_000
}

// BucketTable is the deterministic token table for one numBuckets value.
//
// tokens[i] is the hash-tag content whose CRC16 slot (slots[i]) falls inside
// bucket i's SlotRange(i, len(tokens)). moduloSafe records whether every token
// additionally satisfies slot%numBuckets == i, the interleaved policy Redis
// Cluster itself uses across real nodes -- useful if Dragonfly's internal
// thread-sharding turns out to follow that scheme rather than contiguous blocks.
type BucketTable struct {
	tokens     []string
	slots      []int
	moduloSafe bool
}

// Token panics rather than returning an error because an out-of-range bucket
// index is an internal-invariant violation, never bad user input: every index
// reaching here comes from crc32(fqdn) % numBuckets against the same
// numBuckets this table was built for.
func (t *BucketTable) Token(i int) string {
	if i < 0 || i >= len(t.tokens) {
		panic(fmt.Sprintf("bucket index %d out of range for a table of size %d", i, len(t.tokens)))
	}
	return t.tokens[i]
}

// Computed tables, keyed by numBuckets. ComputeBucketTable runs from
// request-handling goroutines, so two of them may race to fill the same key on
// first access; LoadOrStore makes every caller observe one table, and the
// search being a pure function of numBuckets makes a duplicated computation
// wasteful rather than wrong.
var bucketTables sync.Map

// ComputeBucketTable returns the token table for numBuckets, memoized per process.
//
// Single ascending-counter, first-fit-per-bucket coupon-collector search.
// Candidates are the decimal digits of an increasing counter ("0", "1", "2",
// ...) -- deterministic, seedless, and identical in every language and process
// for the same numBuckets, which is what lets a writer and a reader agree on
// key names with no coordination channel between them.
//
// This candidate order is a frozen contract once shipped: changing it (or the
// CRC16 algorithm) silently produces a different table for the same numBuckets
// and must not happen without a coordinated rewrite of every deployed
// namespace's bucketed tiers.
func ComputeBucketTable(numBuckets int) (*BucketTable, error) {
	if numBuckets < 1 || numBuckets > numSlots {
		return nil, fmt.Errorf("numBuckets must be in [1, %d], got %d", numSlots, numBuckets)
	}
	if cached, ok := bucketTables.Load(numBuckets); ok {
		return cached.(*BucketTable), nil
	}

	starts := make([]int, numBuckets)
	for i := range starts {
		starts[i], _ = SlotRange(i, numBuckets)
	}
	moduloSafe := isModuloSafe(numBuckets)

	tokens := make([]string, numBuckets)
	slots := make([]int, numBuckets)
	filled := 0
	maxDraws := iterationCap(numBuckets)
	for k := 0; filled < numBuckets; k++ {
		if k > maxDraws {
			return nil, fmt.Errorf(
				"ComputeBucketTable(numBuckets=%d): could not fill all buckets within %d "+
					"candidate draws; CRC16 may be broken, or this numBuckets is pathological",
				numBuckets, maxDraws)
		}
		candidate := strconv.Itoa(k)
		s := Slot([]byte(candidate))
		// bisect_right(starts, s) - 1: the block whose start is the last one at
		// or below s.
		bucket := sort.Search(numBuckets, func(i int) bool { return starts[i] > s }) - 1
		if tokens[bucket] != "" || (moduloSafe && s%numBuckets != bucket) {
			continue
		}
		tokens[bucket] = candidate
		slots[bucket] = s
		filled++
	}

	table := &BucketTable{tokens: tokens, slots: slots, moduloSafe: moduloSafe}
	actual, _ := bucketTables.LoadOrStore(numBuckets, table)
	return actual.(*BucketTable), nil
}
