package router

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
)

// db 13 is this package's own suite; db 12 belongs to the Python-generated
// golden fixture (golden_test.go), db 14 to go-benchmark's own suite.
const testDB = 13

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

func newTestStore(t *testing.T) (context.Context, *redis.Client, *NoScriptRuleStore) {
	t.Helper()
	ctx, conn := newTestConn(t)
	store, err := NewNoScriptRuleStore(ctx, conn, "test", 1)
	if err != nil {
		t.Fatalf("NewNoScriptRuleStore: %v", err)
	}
	return ctx, conn, store
}

func mustPut(t *testing.T, store *NoScriptRuleStore, rule Rule) {
	t.Helper()
	if err := store.Put(rule); err != nil {
		t.Fatalf("Put(%+v): %v", rule, err)
	}
}

func mustLookup(t *testing.T, store *NoScriptRuleStore, fqdn1 string, port int, fqdn2 string, mode MatchMode) *Match {
	t.Helper()
	m, err := store.Lookup(fqdn1, port, fqdn2, mode)
	if err != nil {
		t.Fatalf("Lookup(%q, %d, %q, %s): %v", fqdn1, port, fqdn2, mode, err)
	}
	return m
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
