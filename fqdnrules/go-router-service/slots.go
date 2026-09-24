// Deterministic Redis-Cluster-slot-targeted bucket tokens: no Redis, no I/O.
//
// The __global__ and __tld__:<tld> tiers in store.go replicate across
// numBuckets physical keys so lookups spread across worker threads instead of
// hammering one key. This file computes, for a given numBuckets, N distinct
// opaque string tokens such that token i's CRC16 hash slot falls inside
// bucket i's exclusive, equal-sized share of the 16384-slot Redis Cluster
// keyspace -- a guarantee, not a coincidence of whatever slot a literal
// "<tag>:<i>" string happened to land in.
//
// The token table is a pure function of numBuckets alone (see
// ComputeBucketTable), shared by every namespace and every tag family, and is
// never persisted: every process recomputes the identical table. This is a
// direct port of fqdn_slots.py; the candidate search order, CRC16 variant,
// and modulo-safety arithmetic are a frozen cross-language contract with that
// file and must match it exactly.
package router

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
)

// NumSlots is Redis Cluster's fixed hash-slot space.
const NumSlots = 16384

// Crc16 is CRC-16/XMODEM: poly 0x1021, init 0x0000, no reflection, no final
// XOR. This is the exact variant Redis Cluster's HASH_SLOT uses (easily
// confused with CRC-16/CCITT-FALSE, which differs only in init=0xFFFF).
// Known-answer vector: Crc16([]byte("123456789")) == 0x31C3.
func Crc16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc = crc << 1
			}
		}
	}
	return crc
}

// Slot is the 0..16383 Redis Cluster hash slot for raw bytes.
func Slot(data []byte) int {
	return int(Crc16(data)) & 0x3FFF
}

// KeySlot is the Redis-Cluster hash-tag-aware slot for a full key string.
// Hashes only the content of the first non-empty {...} span if one is
// present (matching Redis's own hash-tag extraction); otherwise hashes the
// whole key.
func KeySlot(key string) int {
	start := -1
	for i := 0; i < len(key); i++ {
		if key[i] == '{' {
			start = i
			break
		}
	}
	if start != -1 {
		end := -1
		for i := start + 1; i < len(key); i++ {
			if key[i] == '}' {
				end = i
				break
			}
		}
		if end != -1 && end != start+1 {
			return Slot([]byte(key[start+1 : end]))
		}
	}
	return Slot([]byte(key))
}

// SlotRange returns bucket i's half-open share of the slot space: [lo, hi).
// Floor-boundary partition: boundaries[i] = floor(i * 16384 / N). Range
// widths differ by at most 1 slot; which buckets get the extra slot falls out
// of the floor arithmetic rather than an explicit remainder branch.
func SlotRange(i, numBuckets int) (lo, hi int) {
	lo = (i * NumSlots) / numBuckets
	hi = ((i + 1) * NumSlots) / numBuckets
	return lo, hi
}

// bucketHasModuloMatch reports whether [lo, hi) contains a slot s with
// s % numBuckets == bucketIndex.
//
// HAZARD: bucketIndex-lo is routinely negative, and Go's % returns a
// negative result on a negative left operand (unlike Python's, which
// normalizes to non-negative). Normalize explicitly or moduloSafe flips.
func bucketHasModuloMatch(lo, hi, numBuckets, bucketIndex int) bool {
	diff := (bucketIndex - lo) % numBuckets
	diff = (diff + numBuckets) % numBuckets
	smallest := lo + diff
	return smallest < hi
}

// isModuloSafe reports whether every bucket's contiguous range also contains
// a slot that satisfies the interleaved "slot % num_buckets == bucket_index"
// policy -- pure arithmetic, independent of CRC16.
func isModuloSafe(numBuckets int) bool {
	for i := 0; i < numBuckets; i++ {
		lo, hi := SlotRange(i, numBuckets)
		if !bucketHasModuloMatch(lo, hi, numBuckets, i) {
			return false
		}
	}
	return true
}

// iterationCap is a generous ceiling on candidate draws before
// ComputeBucketTable gives up loudly instead of spinning forever.
//
// Filling N bins by uniform random draws is coupon-collector: expected draws
// ~= N * ln(N). The modulo-safe search additionally requires
// slot % N == bucketIndex, roughly halving each draw's acceptance chance for
// its bucket, so budget a large constant-factor margin over the plain
// estimate rather than model the dual-constraint expectation exactly. 200x
// plus a flat floor comfortably covers both the modulo-safe and
// contiguous-only cases at every legal N, including N close to 16384.
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
// bucket i's SlotRange(i, len(tokens)). moduloSafe records whether every
// token additionally satisfies slot % numBuckets == i, the interleaved
// policy Redis Cluster itself uses across real nodes -- useful if
// Dragonfly's internal thread-sharding turns out to follow that scheme
// rather than contiguous blocks.
type BucketTable struct {
	tokens     []string
	slots      []int
	moduloSafe bool
}

// Token returns the token for bucket i.
//
// Token PANICS on an out-of-range i. This is deliberate and correct: bucket
// indices come from crc32(fqdn) % numBuckets and are in range by
// construction, so an out-of-range index is an internal-invariant
// violation, not user input that could legitimately be out of range.
func (t *BucketTable) Token(i int) string {
	if i < 0 || i >= len(t.tokens) {
		panic(fmt.Sprintf("bucket index %d out of range for a table of size %d", i, len(t.tokens)))
	}
	return t.tokens[i]
}

type bucketTableEntry struct {
	once  sync.Once
	table *BucketTable
	err   error
}

// bucketTableCache is a package-level memo of numBuckets -> *bucketTableEntry.
// Concurrent first access for the same numBuckets is safe: sync.Map.LoadOrStore
// ensures exactly one entry is published per numBuckets, and that entry's
// sync.Once ensures exactly one goroutine computes the table while every
// other caller blocks on Once.Do and then observes the identical result.
var bucketTableCache sync.Map // int -> *bucketTableEntry

// ComputeBucketTable computes the token table for numBuckets, memoized per
// process.
//
// Single ascending-counter, first-fit-per-bucket coupon-collector search.
// Candidates are the decimal digits of an increasing counter ("0", "1", "2",
// ...) -- deterministic, seedless, and identical in every language and
// process for the same numBuckets, which is what lets a writer and a reader
// agree on key names with no coordination channel between them.
//
// This candidate order is a frozen contract shared with fqdn_slots.py:
// changing it (or the CRC16 algorithm) silently produces a different table
// for the same numBuckets and must not happen without a coordinated rewrite
// of every deployed namespace's bucketed tiers.
func ComputeBucketTable(numBuckets int) (*BucketTable, error) {
	// Validated before the memo is consulted, so a bad numBuckets is never
	// cached.
	if numBuckets < 1 || numBuckets > 16384 {
		return nil, fmt.Errorf("numBuckets must be in [1, 16384], got %d", numBuckets)
	}

	actual, _ := bucketTableCache.LoadOrStore(numBuckets, &bucketTableEntry{})
	entry := actual.(*bucketTableEntry)
	entry.once.Do(func() {
		entry.table, entry.err = buildBucketTable(numBuckets)
	})
	return entry.table, entry.err
}

func buildBucketTable(numBuckets int) (*BucketTable, error) {
	starts := make([]int, numBuckets)
	for i := 0; i < numBuckets; i++ {
		lo, _ := SlotRange(i, numBuckets)
		starts[i] = lo
	}
	moduloSafe := isModuloSafe(numBuckets)

	tokens := make([]string, numBuckets)
	slots := make([]int, numBuckets)
	filled := make([]bool, numBuckets)
	numFilled := 0
	drawCap := iterationCap(numBuckets)

	for k := 0; numFilled < numBuckets; k++ {
		if k > drawCap {
			return nil, fmt.Errorf(
				"ComputeBucketTable(numBuckets=%d): could not fill all buckets within %d candidate draws; "+
					"CRC16 may be broken, or this numBuckets is pathological", numBuckets, drawCap)
		}
		candidate := strconv.Itoa(k)
		s := Slot([]byte(candidate))
		bucket := sort.SearchInts(starts, s+1) - 1
		if !filled[bucket] && (!moduloSafe || s%numBuckets == bucket) {
			tokens[bucket] = candidate
			slots[bucket] = s
			filled[bucket] = true
			numFilled++
		}
	}

	return &BucketTable{tokens: tokens, slots: slots, moduloSafe: moduloSafe}, nil
}
