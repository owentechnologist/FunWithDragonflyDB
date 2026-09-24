package main

import (
	"hash/crc32"
	"strings"
	"testing"
)

var bucketProbeFqdns = []string{
	"shop.omega.com",
	"api.omega.net",
	"www.kappa.org",
	"portal.sigma.dev",
	"mail.lambda.io",
}

func TestCrc32MatchesPythonZlib(t *testing.T) {
	cases := []struct {
		input string
		want  uint32
	}{
		{"shop.omega.com", 2637580856},
		{"api.omega.net", 1862906577},
		{"www.kappa.org", 1383770082},
		{"portal.sigma.dev", 452365499},
		{"mail.lambda.io", 987378604},
		{"a.b.c.example.com", 2031253551},
		{"example.com", 3069857465},
		{"*", 163128923},
	}
	for _, c := range cases {
		if got := crc32.ChecksumIEEE([]byte(c.input)); got != c.want {
			t.Errorf("crc32(%q) = %d, want %d", c.input, got, c.want)
		}
	}
}

func TestMultiLevelCandidateTable(t *testing.T) {
	want := []Candidate{
		{Pattern: "a.b.c.example.com", Rank: 0, NeedsMulti: false, Tag: "example.com"},
		{Pattern: "*.b.c.example.com", Rank: 1, NeedsMulti: false, Tag: "example.com"},
		{Pattern: "*.c.example.com", Rank: 2, NeedsMulti: true, Tag: "example.com"},
		{Pattern: "*.example.com", Rank: 3, NeedsMulti: true, Tag: "example.com"},
		{Pattern: "*.com", Rank: 4, NeedsMulti: true, Tag: "__tld__:com"},
		{Pattern: "*", Rank: 255, NeedsMulti: false, Tag: "__global__"},
	}
	got := candidates("a.b.c.example.com", MultiLevel, 1)
	assertCandidates(t, got, want)
}

func TestSingleLevelCandidateTable(t *testing.T) {
	want := []Candidate{
		{Pattern: "a.b.c.example.com", Rank: 0, NeedsMulti: false, Tag: "example.com"},
		{Pattern: "*.b.c.example.com", Rank: 1, NeedsMulti: false, Tag: "example.com"},
		{Pattern: "*", Rank: 255, NeedsMulti: false, Tag: "__global__"},
	}
	got := candidates("a.b.c.example.com", SingleLevel, 1)
	assertCandidates(t, got, want)
}

func assertCandidates(t *testing.T, got, want []Candidate) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d candidates %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// fieldCandidates() must be a faithful drop of candidates()'s Tag field and
// nothing else -- same Pattern/Rank/NeedsMulti sequence, for both modes.
func TestFieldCandidatesMatchCandidatesMinusTag(t *testing.T) {
	const fqdn = "a.b.c.example.com"
	for _, mode := range []MatchMode{SingleLevel, MultiLevel} {
		full := candidates(fqdn, mode, 1)
		fields := fieldCandidates(fqdn, mode)
		if len(full) != len(fields) {
			t.Fatalf("mode %s: candidates() has %d entries, fieldCandidates() has %d", mode, len(full), len(fields))
		}
		for i := range full {
			want := FieldCandidate{Pattern: full[i].Pattern, Rank: full[i].Rank, NeedsMulti: full[i].NeedsMulti}
			if fields[i] != want {
				t.Errorf("mode %s: fieldCandidates()[%d] = %+v, want %+v", mode, i, fields[i], want)
			}
		}
	}
}

func TestSingleLevelCandidatesAreSubsequenceOfMultiLevel(t *testing.T) {
	const fqdn = "a.b.c.example.com"
	single := candidates(fqdn, SingleLevel, 1)
	multi := candidates(fqdn, MultiLevel, 1)
	next := 0
	for _, s := range single {
		for next < len(multi) && multi[next].Pattern != s.Pattern {
			next++
		}
		if next == len(multi) {
			t.Fatalf("single-level pattern %q is not in multi-level order", s.Pattern)
		}
		next++
	}
	if len(single) != 3 || len(multi) != 6 {
		t.Errorf("got %d single and %d multi candidates, want 3 and 6", len(single), len(multi))
	}
}

func TestCandidateRanksStrictlyIncrease(t *testing.T) {
	got := candidates("a.b.c.example.com", MultiLevel, 1)
	want := []int{0, 1, 2, 3, 4, 255}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Rank != w {
			t.Errorf("rank %d = %d, want %d", i, got[i].Rank, w)
		}
	}
}

func TestGlobalTierIsAlwaysLastAndNotDeep(t *testing.T) {
	for _, mode := range []MatchMode{SingleLevel, MultiLevel} {
		cs := candidates("example.com", mode, 1)
		last := cs[len(cs)-1]
		if last.Pattern != "*" || last.Rank != globalRank || last.NeedsMulti {
			t.Errorf("mode %s: last candidate = %+v, want {* 255 false __global__}", mode, last)
		}
		if last.Tag != "__global__" {
			t.Errorf("mode %s: last tag = %q, want %q", mode, last.Tag, "__global__")
		}
	}
}

func TestModeString(t *testing.T) {
	if SingleLevel.String() != "single" {
		t.Errorf("SingleLevel.String() = %q, want %q", SingleLevel.String(), "single")
	}
	if MultiLevel.String() != "multi" {
		t.Errorf("MultiLevel.String() = %q, want %q", MultiLevel.String(), "multi")
	}
}

// The global tier's tag is the same fixed string for every query; only
// BucketIndex spreads the reads across replicas.
func TestGlobalTierBucketIndexVariesWithQueryFqdn(t *testing.T) {
	indexesFor := func(numBuckets int) []int {
		var indexes []int
		for _, f := range bucketProbeFqdns {
			cs := candidates(f, MultiLevel, numBuckets)
			last := cs[len(cs)-1]
			if last.Tag != "__global__" {
				t.Errorf("numBuckets=%d: %s last tag = %q, want %q", numBuckets, f, last.Tag, "__global__")
			}
			indexes = append(indexes, last.BucketIndex)
		}
		return indexes
	}

	cases := []struct {
		numBuckets int
		want       []int
	}{
		{8, []int{0, 1, 2, 3, 4}},
		{4, []int{0, 1, 2, 3, 0}},
		{1, []int{0, 0, 0, 0, 0}},
	}
	for _, c := range cases {
		got := indexesFor(c.numBuckets)
		if len(got) != len(c.want) {
			t.Fatalf("numBuckets=%d produced %d indexes, want %d", c.numBuckets, len(got), len(c.want))
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("numBuckets=%d bucket indexes = %v, want %v", c.numBuckets, got, c.want)
				break
			}
		}
	}
}

// Every candidate carries the query's bucket index, apex ones included; it is
// router.go's key() that ignores it for the tiers that are never bucketed.
func TestBucketedTldTierButUnbucketedApexTier(t *testing.T) {
	cs := candidates("api.omega.net", MultiLevel, 8)
	want := []Candidate{
		{Pattern: "api.omega.net", Rank: 0, NeedsMulti: false, Tag: "omega.net", BucketIndex: 1},
		{Pattern: "*.omega.net", Rank: 1, NeedsMulti: false, Tag: "omega.net", BucketIndex: 1},
		{Pattern: "*.net", Rank: 2, NeedsMulti: true, Tag: "__tld__:net", BucketIndex: 1},
		{Pattern: "*", Rank: 255, NeedsMulti: false, Tag: "__global__", BucketIndex: 1},
	}
	assertCandidates(t, cs, want)
}

func TestHashTag(t *testing.T) {
	cases := []struct{ pattern, want string }{
		{"*", "__global__"},
		{"com", "__tld__:com"},
		{"*.com", "__tld__:com"},
		{"example.com", "example.com"},
		{"*.example.com", "example.com"},
		{"a.b.c.example.com", "example.com"},
		{"*.corp.dragonfly.com", "dragonfly.com"},
	}
	for _, c := range cases {
		if got := hashTag(c.pattern); got != c.want {
			t.Errorf("hashTag(%q) = %q, want %q", c.pattern, got, c.want)
		}
	}
}

func TestIsBucketedTag(t *testing.T) {
	cases := []struct {
		tag  string
		want bool
	}{
		{"__global__", true},
		{"__tld__:com", true},
		{"example.com", false},
		{"dragonfly.com", false},
	}
	for _, c := range cases {
		if got := isBucketedTag(c.tag); got != c.want {
			t.Errorf("isBucketedTag(%q) = %v, want %v", c.tag, got, c.want)
		}
	}
}

func TestNormalizeFqdnAccepts(t *testing.T) {
	cases := []struct{ input, want string }{
		{"API.Foo.COM", "api.foo.com"},
		{"api.foo.com.", "api.foo.com"},
		{"  api.foo.com.  ", "api.foo.com"},
		{"com", "com"},
	}
	for _, c := range cases {
		got, err := normalizeFqdn(c.input)
		if err != nil {
			t.Errorf("normalizeFqdn(%q) returned error %v", c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeFqdn(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestNormalizeFqdnRejects(t *testing.T) {
	tooDeep := make([]string, 0, 26)
	for i := 0; i < 25; i++ {
		tooDeep = append(tooDeep, "l")
	}
	cases := []struct{ name, input string }{
		{"empty", ""},
		{"bare wildcard", "*"},
		{"embedded wildcard", "*.foo.com"},
		{"leading empty label", ".foo.com"},
		{"interior empty label", "api..foo.com"},
		{"too many labels", strings.Join(tooDeep, ".") + ".com"},
	}
	for _, c := range cases {
		got, err := normalizeFqdn(c.input)
		if err == nil {
			t.Errorf("%s: normalizeFqdn(%q) = %q, want an error", c.name, c.input, got)
			continue
		}
		if _, ok := err.(*InvalidFqdnError); !ok {
			t.Errorf("%s: normalizeFqdn(%q) error type = %T, want *InvalidFqdnError", c.name, c.input, err)
		}
	}
}

func TestStorageByteDefaultsToBoth(t *testing.T) {
	var zero WildcardScope
	if got := zero.storageByte(); got != 'B' {
		t.Errorf("zero WildcardScope storage byte = %q, want 'B'", got)
	}
	if got := ScopeSingle.storageByte(); got != 'S' {
		t.Errorf("ScopeSingle storage byte = %q, want 'S'", got)
	}
	if got := ScopeMulti.storageByte(); got != 'M' {
		t.Errorf("ScopeMulti storage byte = %q, want 'M'", got)
	}
	if got := ScopeBoth.storageByte(); got != 'B' {
		t.Errorf("ScopeBoth storage byte = %q, want 'B'", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
