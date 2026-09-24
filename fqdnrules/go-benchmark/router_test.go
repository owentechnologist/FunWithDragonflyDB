package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// db 15 belongs to the Python suite and db 12/13 to other work, so this suite
// owns db 14 alone.
const testDB = 14

func newTestConn(t *testing.T) (context.Context, *redis.Client) {
	t.Helper()
	ctx := context.Background()
	conn := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: testDB})
	if err := conn.Ping(ctx).Err(); err != nil {
		conn.Close()
		t.Skip("no local redis/dragonfly reachable on localhost:6379")
	}
	if err := conn.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushing db %d: %v", testDB, err)
	}
	t.Cleanup(func() {
		if err := conn.FlushDB(ctx).Err(); err != nil {
			t.Errorf("flushing db %d: %v", testDB, err)
		}
		conn.Close()
	})
	return ctx, conn
}

func newTestStore(t *testing.T) (context.Context, *redis.Client, *RuleStore) {
	t.Helper()
	ctx, conn := newTestConn(t)
	store, err := NewRuleStore(ctx, conn, "test", 1)
	if err != nil {
		t.Fatalf("NewRuleStore: %v", err)
	}
	return ctx, conn, store
}

func mustPut(t *testing.T, store *RuleStore, rule Rule) {
	t.Helper()
	if err := store.Put(rule); err != nil {
		t.Fatalf("Put(%+v): %v", rule, err)
	}
}

func mustLookup(t *testing.T, store *RuleStore, fqdn1 string, port int, fqdn2 string, mode MatchMode) *Match {
	t.Helper()
	m, err := store.Lookup(fqdn1, port, fqdn2, mode)
	if err != nil {
		t.Fatalf("Lookup(%q, %d, %q, %s): %v", fqdn1, port, fqdn2, mode, err)
	}
	return m
}

func mustMiss(t *testing.T, store *RuleStore, fqdn1 string, port int, fqdn2 string, mode MatchMode) {
	t.Helper()
	m := mustLookup(t, store, fqdn1, port, fqdn2, mode)
	if m != nil {
		t.Errorf("Lookup(%q, %d, %q, %s) = %+v, want a miss", fqdn1, port, fqdn2, mode, m)
	}
}

func TestLookupRejectsBareWildcardAsConcreteQuery(t *testing.T) {
	_, _, store := newTestStore(t)
	m, err := store.Lookup("*", 443, "backend.com", MultiLevel)
	if m != nil {
		t.Errorf("Lookup returned %+v, want nil", m)
	}
	if _, ok := err.(*InvalidFqdnError); !ok {
		t.Fatalf("Lookup error = %v (%T), want *InvalidFqdnError", err, err)
	}
}

func TestLookupRejectsTooManyLabels(t *testing.T) {
	_, _, store := newTestStore(t)
	parts := make([]string, 25)
	for i := range parts {
		parts[i] = "l"
	}
	tooDeep := strings.Join(parts, ".") + ".com"
	_, err := store.Lookup(tooDeep, 443, "backend.com", MultiLevel)
	if _, ok := err.(*InvalidFqdnError); !ok {
		t.Fatalf("Lookup error = %v (%T), want *InvalidFqdnError", err, err)
	}
}

func TestLookupRejectsPortOutOfRange(t *testing.T) {
	_, _, store := newTestStore(t)
	for _, port := range []int{0, -1, 65536} {
		_, err := store.Lookup("api.foo.com", port, "backend.foo.com", MultiLevel)
		if _, ok := err.(*InvalidFqdnError); !ok {
			t.Errorf("Lookup with port %d: error = %v (%T), want *InvalidFqdnError", port, err, err)
		}
	}
}

func TestExactMatchBothModes(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "api.foo.com", Port: 443, Fqdn2: "backend-api.foo.com", RuleID: "r-exact"})
	for _, mode := range []MatchMode{SingleLevel, MultiLevel} {
		m := mustLookup(t, store, "api.foo.com", 443, "backend-api.foo.com", mode)
		if m == nil {
			t.Fatalf("mode %s: got a miss, want r-exact", mode)
		}
		if m.RuleID != "r-exact" || m.Rank1 != 0 || m.Rank2 != 0 {
			t.Errorf("mode %s: got %+v, want rule r-exact at ranks (0, 0)", mode, m)
		}
	}
}

// Put stores a pattern verbatim, so only the query side is normalized.
func TestLookupNormalizesBothQueryFqdns(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "api.foo.com", Port: 443, Fqdn2: "backend-api.foo.com", RuleID: "r-exact"})
	m := mustLookup(t, store, "  API.Foo.COM.  ", 443, "  Backend-API.Foo.COM.  ", MultiLevel)
	if m == nil {
		t.Fatal("got a miss, want r-exact")
	}
	if m.RuleID != "r-exact" || m.Rank1 != 0 || m.Rank2 != 0 {
		t.Errorf("got %+v, want r-exact at ranks (0, 0)", m)
	}
}

func TestLookupRejectsAnInvalidFqdn2(t *testing.T) {
	_, _, store := newTestStore(t)
	for _, fqdn2 := range []string{"*", "", "backend..foo.com"} {
		m, err := store.Lookup("api.foo.com", 443, fqdn2, MultiLevel)
		if m != nil {
			t.Errorf("Lookup with fqdn2 %q returned %+v, want nil", fqdn2, m)
		}
		if _, ok := err.(*InvalidFqdnError); !ok {
			t.Errorf("Lookup with fqdn2 %q: error = %v (%T), want *InvalidFqdnError", fqdn2, err, err)
		}
	}
}

func TestMissOnWrongFqdn2AndWrongPort(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "api.foo.com", Port: 443, Fqdn2: "backend-api.foo.com", RuleID: "r-exact"})
	mustMiss(t, store, "api.foo.com", 443, "wrong.foo.com", MultiLevel)
	mustMiss(t, store, "api.foo.com", 8080, "backend-api.foo.com", MultiLevel)
}

func TestWorkedExampleExactFqdn2BeatsWildcardFqdn2(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "*.corp.dragonfly.com", Port: 443, Fqdn2: "retailer.com", RuleID: "r-exact-dest"})
	mustPut(t, store, Rule{Fqdn1: "*.corp.dragonfly.com", Port: 443, Fqdn2: "*.com", RuleID: "r-wild-dest"})

	m := mustLookup(t, store, "console.corp.dragonfly.com", 443, "retailer.com", SingleLevel)
	if m == nil {
		t.Fatal("got a miss, want r-exact-dest")
	}
	if m.RuleID != "r-exact-dest" || m.Fqdn1Pattern != "*.corp.dragonfly.com" || m.Fqdn2Pattern != "retailer.com" {
		t.Errorf("got %+v, want r-exact-dest via *.corp.dragonfly.com and retailer.com", m)
	}

	m2 := mustLookup(t, store, "console.corp.dragonfly.com", 443, "shop.com", SingleLevel)
	if m2 == nil {
		t.Fatal("got a miss, want r-wild-dest")
	}
	if m2.RuleID != "r-wild-dest" || m2.Fqdn2Pattern != "*.com" {
		t.Errorf("got %+v, want r-wild-dest via *.com", m2)
	}
}

func TestSingleLevelRejectsDescendantTwoLevelsDown(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "*.corp.dragonfly.com", Port: 443, Fqdn2: "retailer.com", RuleID: "r-corp"})
	mustMiss(t, store, "a.console.corp.dragonfly.com", 443, "retailer.com", SingleLevel)
}

func TestMultiLevelAcceptsDescendantTwoLevelsDown(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "*.corp.dragonfly.com", Port: 443, Fqdn2: "retailer.com", RuleID: "r-corp"})
	m := mustLookup(t, store, "a.console.corp.dragonfly.com", 443, "retailer.com", MultiLevel)
	if m == nil {
		t.Fatal("got a miss, want r-corp")
	}
	if m.RuleID != "r-corp" || m.Rank1 != 2 {
		t.Errorf("got %+v, want r-corp at Rank1 2", m)
	}
}

func TestMultiLevelLongestMatchWinsOverBroaderAncestor(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "*.dragonfly.com", Port: 443, Fqdn2: "retailer.com", RuleID: "r-broad"})
	mustPut(t, store, Rule{Fqdn1: "*.corp.dragonfly.com", Port: 443, Fqdn2: "retailer.com", RuleID: "r-narrow"})
	m := mustLookup(t, store, "console.corp.dragonfly.com", 443, "retailer.com", MultiLevel)
	if m == nil {
		t.Fatal("got a miss, want r-narrow")
	}
	if m.RuleID != "r-narrow" || m.Fqdn1Pattern != "*.corp.dragonfly.com" || m.Rank1 != 1 {
		t.Errorf("got %+v, want r-narrow via *.corp.dragonfly.com at Rank1 1", m)
	}
}

func TestGlobalFallbackReachedOnlyWhenNothingMoreSpecificMatches(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "api.foo.com", Port: 443, Fqdn2: "backend.foo.com", RuleID: "r-specific"})
	mustPut(t, store, Rule{Fqdn1: "*", Port: 443, Fqdn2: "catch-all.example.com", RuleID: "r-global"})

	m1 := mustLookup(t, store, "api.foo.com", 443, "backend.foo.com", MultiLevel)
	if m1 == nil || m1.RuleID != "r-specific" || m1.Fqdn1Pattern != "api.foo.com" {
		t.Errorf("got %+v, want r-specific via api.foo.com", m1)
	}

	m2 := mustLookup(t, store, "api.foo.com", 443, "catch-all.example.com", MultiLevel)
	if m2 == nil {
		t.Fatal("got a miss, want r-global")
	}
	if m2.RuleID != "r-global" || m2.Fqdn1Pattern != "*" || m2.Rank1 != globalRank {
		t.Errorf("got %+v, want r-global via * at Rank1 255", m2)
	}
}

// The apex, per-TLD and global tiers are three separate hash-tag groups, each
// dispatched as its own EVALSHA, so the overall winner is picked on the Go side
// rather than by the script. This is the only scenario where more than one group
// returns a hit for the same query.
func TestCrossGroupWinnerIsTheMostSpecificHit(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "*.b.example.com", Port: 443, Fqdn2: "*", RuleID: "r-apex"})
	mustPut(t, store, Rule{Fqdn1: "*.com", Port: 443, Fqdn2: "*", RuleID: "r-tld"})
	mustPut(t, store, Rule{Fqdn1: "*", Port: 443, Fqdn2: "*", RuleID: "r-global"})

	ladder := []struct {
		wantRuleID  string
		wantPattern string
		wantRank1   int
	}{
		{"r-apex", "*.b.example.com", 1},
		{"r-tld", "*.com", 3},
		{"r-global", "*", 255},
	}
	for _, step := range ladder {
		m := mustLookup(t, store, "a.b.example.com", 443, "dest.example.net", MultiLevel)
		if m == nil {
			t.Fatalf("got a miss, want %s", step.wantRuleID)
		}
		if m.RuleID != step.wantRuleID || m.Fqdn1Pattern != step.wantPattern || m.Rank1 != step.wantRank1 {
			t.Errorf("got %+v, want %s via %s at Rank1 %d", m, step.wantRuleID, step.wantPattern, step.wantRank1)
		}
		if m.Rank2 != globalRank || m.Fqdn2Pattern != "*" {
			t.Errorf("got fqdn2 %q at Rank2 %d, want * at Rank2 255", m.Fqdn2Pattern, m.Rank2)
		}
		deleted, err := store.Delete(step.wantPattern, 443, "*")
		if err != nil {
			t.Fatalf("Delete(%q): %v", step.wantPattern, err)
		}
		if !deleted {
			t.Fatalf("Delete(%q) = false, want true", step.wantPattern)
		}
	}
	mustMiss(t, store, "a.b.example.com", 443, "dest.example.net", MultiLevel)
}

func TestSingleScopeRuleInvisibleAtDeepRankButVisibleShallow(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "*.dragonfly.com", Port: 443, Fqdn2: "*", RuleID: "r-tls-strict", Scope: ScopeSingle})

	shallow := mustLookup(t, store, "host1.dragonfly.com", 443, "anything.net", MultiLevel)
	if shallow == nil {
		t.Fatal("rank-1 query got a miss, want r-tls-strict")
	}
	if shallow.RuleID != "r-tls-strict" || shallow.Rank1 != 1 {
		t.Errorf("rank-1 query got %+v, want r-tls-strict at Rank1 1", shallow)
	}

	mustMiss(t, store, "a.b.dragonfly.com", 443, "anything.net", MultiLevel)
}

func TestMultiScopeRuleInvisibleAtShallowRankButVisibleDeep(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "*.dragonfly.com", Port: 443, Fqdn2: "*", RuleID: "r-deep-only", Scope: ScopeMulti})

	mustMiss(t, store, "host1.dragonfly.com", 443, "anything.net", MultiLevel)

	deep := mustLookup(t, store, "a.b.dragonfly.com", 443, "anything.net", MultiLevel)
	if deep == nil {
		t.Fatal("rank-2 query got a miss, want r-deep-only")
	}
	if deep.RuleID != "r-deep-only" || deep.Rank1 != 2 {
		t.Errorf("rank-2 query got %+v, want r-deep-only at Rank1 2", deep)
	}
}

func TestExactMatchIsNeverScopeFiltered(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "host9.gamma.io", Port: 8443, Fqdn2: "backend9.gamma.io", RuleID: "r-exact-multi-scope", Scope: ScopeMulti})

	hit := mustLookup(t, store, "host9.gamma.io", 8443, "backend9.gamma.io", MultiLevel)
	if hit == nil {
		t.Fatal("exact-exact query got a miss, want r-exact-multi-scope despite its ScopeMulti declaration")
	}
	if hit.RuleID != "r-exact-multi-scope" || hit.Rank1 != 0 || hit.Rank2 != 0 {
		t.Errorf("exact-exact query got %+v, want r-exact-multi-scope at (Rank1 0, Rank2 0)", hit)
	}
}

func TestShardsIsolateDomainsWithIdenticalRuleShapes(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "api.alpha.com", Port: 443, Fqdn2: "backend-api.alpha.com", RuleID: "r-shared-id"})
	mustPut(t, store, Rule{Fqdn1: "api.beta.com", Port: 443, Fqdn2: "backend-api.beta.com", RuleID: "r-shared-id"})
	mustMiss(t, store, "api.alpha.com", 443, "backend-api.beta.com", MultiLevel)
	mustMiss(t, store, "api.beta.com", 443, "backend-api.alpha.com", MultiLevel)
}

func TestDeleteIsIdempotent(t *testing.T) {
	_, _, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "api.foo.com", Port: 443, Fqdn2: "backend.foo.com", RuleID: "r-x"})

	deleted, err := store.Delete("api.foo.com", 443, "backend.foo.com")
	if err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if !deleted {
		t.Error("first Delete = false, want true")
	}

	deleted, err = store.Delete("api.foo.com", 443, "backend.foo.com")
	if err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if deleted {
		t.Error("second Delete = true, want false")
	}

	mustMiss(t, store, "api.foo.com", 443, "backend.foo.com", MultiLevel)
}

func TestPutIsIdempotentOnReplay(t *testing.T) {
	ctx, conn, store := newTestStore(t)
	rule := Rule{Fqdn1: "api.foo.com", Port: 443, Fqdn2: "backend.foo.com", RuleID: "r-x"}
	mustPut(t, store, rule)
	mustPut(t, store, rule)

	m := mustLookup(t, store, "api.foo.com", 443, "backend.foo.com", MultiLevel)
	if m == nil || m.RuleID != "r-x" {
		t.Fatalf("got %+v, want r-x", m)
	}
	n, err := conn.HLen(ctx, "r:{test:foo.com}:api.foo.com:443").Result()
	if err != nil {
		t.Fatalf("HLEN: %v", err)
	}
	if n != 1 {
		t.Errorf("hash holds %d fields after a replay, want 1", n)
	}
}

func TestDefaultStoreWritesUnbucketedKeyNameAndStillMatches(t *testing.T) {
	ctx, conn, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "api.foo.com", Port: 443, Fqdn2: "backend-api.foo.com", RuleID: "r-exact"})

	keys, err := conn.Keys(ctx, "r:{test:*").Result()
	if err != nil {
		t.Fatalf("KEYS: %v", err)
	}
	sort.Strings(keys)
	want := []string{"r:{test:foo.com}:api.foo.com:443"}
	if !equalStrings(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}

	for _, mode := range []MatchMode{SingleLevel, MultiLevel} {
		m := mustLookup(t, store, "api.foo.com", 443, "backend-api.foo.com", mode)
		if m == nil || m.RuleID != "r-exact" || m.Rank1 != 0 || m.Rank2 != 0 {
			t.Errorf("mode %s: got %+v, want r-exact at ranks (0, 0)", mode, m)
		}
	}
}

func TestBucketedPutWritesGlobalRuleIntoFourReplicaKeys(t *testing.T) {
	ctx, conn := newTestConn(t)
	store, err := NewRuleStore(ctx, conn, "buckets", 4)
	if err != nil {
		t.Fatalf("NewRuleStore: %v", err)
	}
	mustPut(t, store, Rule{Fqdn1: "*", Port: 443, Fqdn2: "*", RuleID: "rule:global"})

	table, err := ComputeBucketTable(4)
	if err != nil {
		t.Fatalf("ComputeBucketTable(4): %v", err)
	}
	want := make([]string, 4)
	for i := range want {
		want[i] = fmt.Sprintf("r:buckets:{%s}:__global__:%d:*:443", table.Token(i), i)
	}
	sort.Strings(want)
	// The bucketed tiers keep the namespace outside the braces, so a plain
	// prefix scan still enumerates them; the apex tier's r:{ns:...} keys and
	// the meta key deliberately fall outside this pattern.
	keys, err := conn.Keys(ctx, "r:buckets:*").Result()
	if err != nil {
		t.Fatalf("KEYS: %v", err)
	}
	sort.Strings(keys)
	if !equalStrings(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for _, key := range want {
		got, err := conn.HGet(ctx, key, "*").Result()
		if err != nil {
			t.Fatalf("HGET %s *: %v", key, err)
		}
		if got != "Brule:global" {
			t.Errorf("HGET %s * = %q, want %q", key, got, "Brule:global")
		}
	}

	mustPut(t, store, Rule{Fqdn1: "api.foo.com", Port: 443, Fqdn2: "backend.foo.com", RuleID: "rule:apex"})
	apexKeys, err := conn.Keys(ctx, "r:{buckets:foo.com*").Result()
	if err != nil {
		t.Fatalf("KEYS: %v", err)
	}
	wantApex := []string{"r:{buckets:foo.com}:api.foo.com:443"}
	if !equalStrings(apexKeys, wantApex) {
		t.Errorf("apex keys = %v, want %v", apexKeys, wantApex)
	}
}

// numBuckets=1 must leave the catch-all tiers on the pre-bucketing key names,
// byte for byte, so an existing unbucketed dataset stays readable.
func TestUnbucketedStoreKeepsTheOriginalCatchAllKeyNames(t *testing.T) {
	ctx, conn, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "*", Port: 443, Fqdn2: "*", RuleID: "rule:global"})
	mustPut(t, store, Rule{Fqdn1: "*.com", Port: 8443, Fqdn2: "*", RuleID: "rule:tld"})

	keys, err := conn.Keys(ctx, "r:*").Result()
	if err != nil {
		t.Fatalf("KEYS: %v", err)
	}
	sort.Strings(keys)
	want := []string{"r:{test:__global__}:*:443", "r:{test:__tld__:com}:*.com:8443"}
	if !equalStrings(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
}

// The point of the token: every key a single query declares for a bucketed
// tier must hash into that query's own bucket's exclusive slot range, so the
// replicas really do land on different Dragonfly threads.
func TestBucketedKeysLandInTheirOwnBucketsSlotRange(t *testing.T) {
	ctx, conn := newTestConn(t)
	const numBuckets = 8
	store, err := NewRuleStore(ctx, conn, "buckets", numBuckets)
	if err != nil {
		t.Fatalf("NewRuleStore: %v", err)
	}
	for _, fqdn := range bucketProbeFqdns {
		for _, c := range candidates(fqdn, MultiLevel, numBuckets) {
			if !isBucketedTag(c.Tag) {
				continue
			}
			key := store.key(c.Tag, c.BucketIndex, c.Pattern, 443)
			lo, hi := SlotRange(c.BucketIndex, numBuckets)
			if got := KeySlot(key); got < lo || got >= hi {
				t.Errorf("%s: key %q hashes to slot %d, outside bucket %d's range [%d, %d)",
					fqdn, key, got, c.BucketIndex, lo, hi)
			}
		}
	}
}

func TestBucketedGlobalRuleMatchesWhicheverBucketTheQueryHashesTo(t *testing.T) {
	ctx, conn := newTestConn(t)
	store, err := NewRuleStore(ctx, conn, "buckets", 4)
	if err != nil {
		t.Fatalf("NewRuleStore: %v", err)
	}
	mustPut(t, store, Rule{Fqdn1: "*", Port: 443, Fqdn2: "*", RuleID: "rule:global"})

	for _, fqdn := range bucketProbeFqdns {
		m := mustLookup(t, store, fqdn, 443, "backend.example.net", MultiLevel)
		if m == nil {
			t.Errorf("%s missed its global-tier replica", fqdn)
			continue
		}
		if m.RuleID != "rule:global" || m.Fqdn1Pattern != "*" || m.Fqdn2Pattern != "*" {
			t.Errorf("%s: got %+v, want rule:global via * and *", fqdn, m)
		}
	}
}

func TestBucketedDeleteClearsEveryReplica(t *testing.T) {
	ctx, conn := newTestConn(t)
	store, err := NewRuleStore(ctx, conn, "buckets", 4)
	if err != nil {
		t.Fatalf("NewRuleStore: %v", err)
	}
	mustPut(t, store, Rule{Fqdn1: "*", Port: 443, Fqdn2: "*", RuleID: "rule:global"})

	deleted, err := store.Delete("*", 443, "*")
	if err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if !deleted {
		t.Error("first Delete = false, want true")
	}

	keys, err := conn.Keys(ctx, "r:buckets:*").Result()
	if err != nil {
		t.Fatalf("KEYS: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("keys after delete = %v, want none", keys)
	}

	deleted, err = store.Delete("*", 443, "*")
	if err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if deleted {
		t.Error("second Delete = true, want false")
	}

	for _, fqdn := range bucketProbeFqdns {
		mustMiss(t, store, fqdn, 443, "backend.example.net", MultiLevel)
	}
}

func TestReopeningNamespaceUnderDifferentNumBucketsFails(t *testing.T) {
	ctx, conn := newTestConn(t)
	store, err := NewRuleStore(ctx, conn, "buckets", 4)
	if err != nil {
		t.Fatalf("NewRuleStore: %v", err)
	}
	mustPut(t, store, Rule{Fqdn1: "*", Port: 443, Fqdn2: "*", RuleID: "rule:global"})

	drifted, err := NewRuleStore(ctx, conn, "buckets", 8)
	if err == nil {
		t.Fatalf("NewRuleStore with num_buckets=8 returned %+v, want an error", drifted)
	}
	if !strings.Contains(err.Error(), "num_buckets=8") {
		t.Errorf("error %q does not mention num_buckets=8", err.Error())
	}
	if !strings.Contains(err.Error(), "num_buckets=4") {
		t.Errorf("error %q does not mention the recorded num_buckets=4", err.Error())
	}

	same, err := NewRuleStore(ctx, conn, "buckets", 4)
	if err != nil {
		t.Fatalf("reopening at num_buckets=4: %v", err)
	}
	m := mustLookup(t, same, "shop.omega.com", 443, "backend.example.net", MultiLevel)
	if m == nil || m.RuleID != "rule:global" {
		t.Errorf("got %+v, want rule:global", m)
	}
}

func TestUnbucketedStoreDoesNotWriteTheMetaKey(t *testing.T) {
	ctx, conn, store := newTestStore(t)
	mustPut(t, store, Rule{Fqdn1: "*", Port: 443, Fqdn2: "*", RuleID: "rule:global"})
	if err := store.PutMany([]Rule{{Fqdn1: "*", Port: 80, Fqdn2: "*", RuleID: "rule:global-80"}}); err != nil {
		t.Fatalf("PutMany: %v", err)
	}
	n, err := conn.Exists(ctx, "r:{test:__meta__}:bucketing").Result()
	if err != nil {
		t.Fatalf("EXISTS: %v", err)
	}
	if n != 0 {
		t.Error("unbucketed store wrote the bucketing meta key, want it absent")
	}
}

func TestPutManyFansOutAndRecordsTheBucketCount(t *testing.T) {
	ctx, conn := newTestConn(t)
	store, err := NewRuleStore(ctx, conn, "buckets", 4)
	if err != nil {
		t.Fatalf("NewRuleStore: %v", err)
	}
	rules := []Rule{
		{Fqdn1: "*", Port: 443, Fqdn2: "*", RuleID: "rule:global"},
		{Fqdn1: "api.foo.com", Port: 443, Fqdn2: "backend.foo.com", RuleID: "rule:apex"},
	}
	if err := store.PutMany(rules); err != nil {
		t.Fatalf("PutMany: %v", err)
	}
	recorded, err := conn.HGet(ctx, "r:{buckets:__meta__}:bucketing", "num_buckets").Result()
	if err != nil {
		t.Fatalf("HGET meta: %v", err)
	}
	if recorded != "4" {
		t.Errorf("recorded num_buckets = %q, want %q", recorded, "4")
	}
	m := mustLookup(t, store, "api.foo.com", 443, "backend.foo.com", MultiLevel)
	if m == nil || m.RuleID != "rule:apex" || m.Rank1 != 0 || m.Rank2 != 0 {
		t.Errorf("apex lookup got %+v, want rule:apex at ranks (0, 0)", m)
	}
}

func TestRecommendedNumBuckets(t *testing.T) {
	ctx, conn := newTestConn(t)
	got := RecommendedNumBuckets(ctx, conn, 16)
	if got < 1 {
		t.Fatalf("RecommendedNumBuckets = %d, want a positive count", got)
	}
	info, err := conn.Info(ctx, "server").Result()
	if err != nil {
		t.Fatalf("INFO server: %v", err)
	}
	if !strings.Contains(info, "thread_count:") && got != 16 {
		t.Errorf("server reports no thread_count, so the default 16 was expected, got %d", got)
	}
}

func TestDemo(t *testing.T) {
	ctx, conn := newTestConn(t)
	store, err := NewRuleStore(ctx, conn, "demo", 1)
	if err != nil {
		t.Fatalf("NewRuleStore: %v", err)
	}
	mustPut(t, store, Rule{Fqdn1: "*.corp.dragonfly.com", Port: 443, Fqdn2: "retailer.com", RuleID: "rule:corp-to-retailer"})
	mustPut(t, store, Rule{Fqdn1: "*.corp.dragonfly.com", Port: 443, Fqdn2: "*.com", RuleID: "rule:corp-to-any-dot-com"})
	mustPut(t, store, Rule{Fqdn1: "*.dragonfly.com", Port: 443, Fqdn2: "*", RuleID: "rule:tls-strict", Scope: ScopeSingle})

	cases := []struct {
		label      string
		fqdn1      string
		port       int
		fqdn2      string
		mode       MatchMode
		wantRuleID string
	}{
		{"single-level exact win over wildcard", "console.corp.dragonfly.com", 443, "retailer.com", SingleLevel, "rule:corp-to-retailer"},
		{"single-level falls through to *.com wildcard", "console.corp.dragonfly.com", 443, "shop.com", SingleLevel, "rule:corp-to-any-dot-com"},
		{"too deep for single-level", "a.console.corp.dragonfly.com", 443, "retailer.com", SingleLevel, ""},
		{"same query, multi-level mode", "a.console.corp.dragonfly.com", 443, "retailer.com", MultiLevel, "rule:corp-to-retailer"},
		{"SINGLE-scoped rule invisible at deep rank", "a.b.dragonfly.com", 443, "anything.net", MultiLevel, ""},
	}
	for _, c := range cases {
		m := mustLookup(t, store, c.fqdn1, c.port, c.fqdn2, c.mode)
		if c.wantRuleID == "" {
			if m != nil {
				t.Errorf("%s: got %+v, want a miss", c.label, m)
			}
			continue
		}
		if m == nil {
			t.Errorf("%s: got a miss, want %s", c.label, c.wantRuleID)
			continue
		}
		if m.RuleID != c.wantRuleID {
			t.Errorf("%s: got %s, want %s", c.label, m.RuleID, c.wantRuleID)
		}
	}
}
