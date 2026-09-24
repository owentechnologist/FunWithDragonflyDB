package main

import (
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"testing"
)

// The frozen cross-language fixture, emitted by `python3 fqdn_slots.py
// --emit-golden` and shared by the Python reference and both Go ports. Any
// divergence here means deployed key names would differ between a Python
// writer and a Go reader against the same Dragonfly.
const slotGoldenPath = "../testdata/slot_tokens_golden.json"

type slotGolden struct {
	Crc16KnownAnswer struct {
		Input    string `json:"input"`
		Crc16    uint16 `json:"crc16"`
		Crc16Hex string `json:"crc16_hex"`
	} `json:"crc16_known_answer"`
	NumBuckets map[string]struct {
		Tokens     []string `json:"tokens"`
		Slots      []int    `json:"slots"`
		ModuloSafe bool     `json:"modulo_safe"`
	} `json:"num_buckets"`
}

func loadSlotGolden(t *testing.T) slotGolden {
	t.Helper()
	raw, err := os.ReadFile(slotGoldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v", slotGoldenPath, err)
	}
	var golden slotGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parsing %s: %v", slotGoldenPath, err)
	}
	return golden
}

func TestCrc16KnownAnswer(t *testing.T) {
	if got := Crc16([]byte("123456789")); got != 0x31C3 {
		t.Fatalf("Crc16(\"123456789\") = 0x%04X, want 0x31C3", got)
	}
}

func TestCrc16MatchesGoldenKnownAnswer(t *testing.T) {
	golden := loadSlotGolden(t)
	if got := Crc16([]byte(golden.Crc16KnownAnswer.Input)); got != golden.Crc16KnownAnswer.Crc16 {
		t.Errorf("Crc16(%q) = %d, want %d (%s)",
			golden.Crc16KnownAnswer.Input, got, golden.Crc16KnownAnswer.Crc16, golden.Crc16KnownAnswer.Crc16Hex)
	}
}

func TestComputeBucketTableMatchesGolden(t *testing.T) {
	golden := loadSlotGolden(t)
	if len(golden.NumBuckets) == 0 {
		t.Fatalf("%s carries no num_buckets entries", slotGoldenPath)
	}
	for key, want := range golden.NumBuckets {
		n, err := strconv.Atoi(key)
		if err != nil {
			t.Fatalf("num_buckets key %q is not an integer: %v", key, err)
		}
		table, err := ComputeBucketTable(n)
		if err != nil {
			t.Errorf("ComputeBucketTable(%d): %v", n, err)
			continue
		}
		if table.moduloSafe != want.ModuloSafe {
			t.Errorf("numBuckets=%d moduloSafe = %v, want %v", n, table.moduloSafe, want.ModuloSafe)
		}
		if len(table.tokens) != len(want.Tokens) {
			t.Errorf("numBuckets=%d produced %d tokens, want %d", n, len(table.tokens), len(want.Tokens))
			continue
		}
		for i := range want.Tokens {
			if table.Token(i) != want.Tokens[i] {
				t.Errorf("numBuckets=%d token[%d] = %q, want %q", n, i, table.Token(i), want.Tokens[i])
			}
			if table.slots[i] != want.Slots[i] {
				t.Errorf("numBuckets=%d slot[%d] = %d, want %d", n, i, table.slots[i], want.Slots[i])
			}
			// The fixture is a frozen record of a claim this asserts directly:
			// every token's slot really does land in its own bucket's range.
			if got := Slot([]byte(want.Tokens[i])); got != want.Slots[i] {
				t.Errorf("numBuckets=%d Slot(%q) = %d, want %d", n, want.Tokens[i], got, want.Slots[i])
			}
			lo, hi := SlotRange(i, n)
			if want.Slots[i] < lo || want.Slots[i] >= hi {
				t.Errorf("numBuckets=%d slot %d is outside bucket %d's range [%d, %d)", n, want.Slots[i], i, lo, hi)
			}
			if want.ModuloSafe && want.Slots[i]%n != i {
				t.Errorf("numBuckets=%d claims moduloSafe but slot %d %% %d = %d, want %d",
					n, want.Slots[i], n, want.Slots[i]%n, i)
			}
		}
	}
}

// The fixture stops at 192, so these pin the two bucket counts whose behaviour
// the fixture cannot show: the exact first N where the interleaved policy stops
// being satisfiable for every bucket, and the largest legal N. Expectations
// come from running fqdn_slots.compute_bucket_table in Python, not from this
// implementation.
func TestBucketTablesMatchPythonBeyondTheFixture(t *testing.T) {
	cases := []struct {
		numBuckets int
		moduloSafe bool
		firstToken string
		firstSlot  int
		lastToken  string
		lastSlot   int
	}{
		{130, false, "14", 115, "44", 16262},
		{16384, true, "3560", 0, "39296", 16383},
	}
	for _, c := range cases {
		table, err := ComputeBucketTable(c.numBuckets)
		if err != nil {
			t.Errorf("ComputeBucketTable(%d): %v", c.numBuckets, err)
			continue
		}
		if table.moduloSafe != c.moduloSafe {
			t.Errorf("numBuckets=%d moduloSafe = %v, want %v", c.numBuckets, table.moduloSafe, c.moduloSafe)
		}
		if table.Token(0) != c.firstToken || table.slots[0] != c.firstSlot {
			t.Errorf("numBuckets=%d bucket 0 = (%q, %d), want (%q, %d)",
				c.numBuckets, table.Token(0), table.slots[0], c.firstToken, c.firstSlot)
		}
		last := c.numBuckets - 1
		if table.Token(last) != c.lastToken || table.slots[last] != c.lastSlot {
			t.Errorf("numBuckets=%d bucket %d = (%q, %d), want (%q, %d)",
				c.numBuckets, last, table.Token(last), table.slots[last], c.lastToken, c.lastSlot)
		}
	}
}

func TestSlotRangesPartitionTheWholeKeyspace(t *testing.T) {
	for _, n := range []int{1, 2, 7, 8, 12, 16, 37, 64, 128, 129, 192, 16384} {
		prev := 0
		for i := 0; i < n; i++ {
			lo, hi := SlotRange(i, n)
			if lo != prev {
				t.Fatalf("numBuckets=%d bucket %d starts at %d, want %d", n, i, lo, prev)
			}
			if hi <= lo {
				t.Fatalf("numBuckets=%d bucket %d is empty: [%d, %d)", n, i, lo, hi)
			}
			prev = hi
		}
		if prev != 16384 {
			t.Errorf("numBuckets=%d ranges end at %d, want 16384", n, prev)
		}
	}
}

func TestKeySlotUsesTheHashTagWhenPresent(t *testing.T) {
	cases := []struct{ key, hashed string }{
		{"r:bench:{42}:__global__:3:*:443", "42"},
		{"r:{bench:foo.com}:api.foo.com:443", "bench:foo.com"},
		{"no braces here", "no braces here"},
		{"empty{}tag", "empty{}tag"}, // an empty span is not a hash tag
		{"unclosed{tag", "unclosed{tag"},
		{"a{b{c}d", "b{c"},   // the first "}" closes the span, brace or not
		{"{}x{y}", "{}x{y}"}, // an empty first span is not retried later
	}
	for _, c := range cases {
		if got, want := KeySlot(c.key), Slot([]byte(c.hashed)); got != want {
			t.Errorf("KeySlot(%q) = %d, want Slot(%q) = %d", c.key, got, c.hashed, want)
		}
	}
}

func TestComputeBucketTableRejectsOutOfRangeSizes(t *testing.T) {
	for _, n := range []int{-1, 0, 16385} {
		if table, err := ComputeBucketTable(n); err == nil {
			t.Errorf("ComputeBucketTable(%d) = %+v, want an error", n, table)
		}
	}
}

func TestTokenPanicsOnAnOutOfRangeBucketIndex(t *testing.T) {
	table, err := ComputeBucketTable(4)
	if err != nil {
		t.Fatalf("ComputeBucketTable(4): %v", err)
	}
	for _, i := range []int{-1, 4} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Token(%d) returned normally, want a panic", i)
				}
			}()
			table.Token(i)
		}()
	}
}

// Concurrent first access to the same numBuckets must hand every goroutine one
// table, not a half-built or divergent one: ComputeBucketTable is called from
// request-handling goroutines.
func TestComputeBucketTableIsSafeUnderConcurrentFirstAccess(t *testing.T) {
	const numBuckets = 97
	var wg sync.WaitGroup
	results := make([]*BucketTable, 32)
	errs := make([]error, len(results))
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = ComputeBucketTable(numBuckets)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Fatalf("goroutine %d got a different *BucketTable than goroutine 0", i)
		}
	}
	for i := 0; i < numBuckets; i++ {
		lo, hi := SlotRange(i, numBuckets)
		if s := results[0].slots[i]; s < lo || s >= hi {
			t.Errorf("token %d's slot %d is outside [%d, %d)", i, s, lo, hi)
		}
	}
}
