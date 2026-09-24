package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// Replays the rule set and query set that crosscheck_python.py ran through the
// Python RuleStore, and asserts the Go port produces the same physical keys and
// the same match for every query. The Python side owns namespace "xpy" on the
// same db and this side owns "xgo", so neither reads the other's keys.

const goldenPathEnv = "FQDN_GOLDEN"

type goldenRule struct {
	Fqdn1  string `json:"fqdn1"`
	Port   int    `json:"port"`
	Fqdn2  string `json:"fqdn2"`
	RuleID string `json:"rule_id"`
	Scope  string `json:"scope"`
}

type goldenQuery struct {
	Fqdn1 string `json:"fqdn1"`
	Port  int    `json:"port"`
	Fqdn2 string `json:"fqdn2"`
	Mode  string `json:"mode"`
}

type goldenMatch struct {
	RuleID       string `json:"rule_id"`
	Fqdn1Pattern string `json:"fqdn1_pattern"`
	Port         int    `json:"port"`
	Fqdn2Pattern string `json:"fqdn2_pattern"`
	Rank1        int    `json:"rank1"`
	Rank2        int    `json:"rank2"`
}

type goldenPayload struct {
	DB         int            `json:"db"`
	NumBuckets int            `json:"num_buckets"`
	Rules      []goldenRule   `json:"rules"`
	Queries    []goldenQuery  `json:"queries"`
	Expected   []*goldenMatch `json:"expected"`
}

func TestGoldenCrossCheckAgainstPython(t *testing.T) {
	path := os.Getenv(goldenPathEnv)
	if path == "" {
		t.Skipf("set %s to the JSON emitted by crosscheck_python.py", goldenPathEnv)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file: %v", err)
	}
	var golden goldenPayload
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parsing golden file: %v", err)
	}

	ctx := context.Background()
	conn := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: golden.DB})
	defer conn.Close()
	if err := conn.Ping(ctx).Err(); err != nil {
		t.Skipf("no redis/dragonfly on localhost:6379: %v", err)
	}
	clearNamespace(t, ctx, conn, "xgo")
	// Set FQDN_KEEP=1 to leave the replayed keys behind for inspection.
	if os.Getenv("FQDN_KEEP") == "" {
		defer clearNamespace(t, ctx, conn, "xgo")
	}

	store, err := NewRuleStore(ctx, conn, "xgo", golden.NumBuckets)
	if err != nil {
		t.Fatalf("NewRuleStore: %v", err)
	}

	scopes := map[string]WildcardScope{"S": ScopeSingle, "M": ScopeMulti, "B": ScopeBoth}
	rules := make([]Rule, len(golden.Rules))
	for i, r := range golden.Rules {
		scope, ok := scopes[r.Scope]
		if !ok {
			t.Fatalf("rule %d has unknown scope %q", i, r.Scope)
		}
		rules[i] = Rule{Fqdn1: r.Fqdn1, Port: r.Port, Fqdn2: r.Fqdn2, RuleID: r.RuleID, Scope: scope}
	}
	if err := store.PutMany(rules); err != nil {
		t.Fatalf("PutMany: %v", err)
	}

	assertSameKeyLayout(t, ctx, conn)

	// Reading the Python-written namespace proves the two implementations are
	// wire-compatible, not merely consistent with each other in parallel.
	pyStore, err := NewRuleStore(ctx, conn, "xpy", golden.NumBuckets)
	if err != nil {
		t.Fatalf("opening the python-written namespace: %v", err)
	}
	runGoldenQueries(t, pyStore, golden, "go reading python-written keys")
	runGoldenQueries(t, store, golden, "go reading go-written keys")
}

func runGoldenQueries(t *testing.T, store *RuleStore, golden goldenPayload, what string) {
	t.Helper()
	modes := map[string]MatchMode{"single": SingleLevel, "multi": MultiLevel}
	for i, q := range golden.Queries {
		mode, ok := modes[q.Mode]
		if !ok {
			t.Fatalf("query %d has unknown mode %q", i, q.Mode)
		}
		got, err := store.Lookup(q.Fqdn1, q.Port, q.Fqdn2, mode)
		if err != nil {
			t.Fatalf("query %d (%s %s:%d -> %s): Lookup: %v", i, q.Mode, q.Fqdn1, q.Port, q.Fqdn2, err)
		}
		want := golden.Expected[i]
		label := what + ": " + describeQuery(i, q)
		if want == nil {
			if got != nil {
				t.Errorf("%s: python says MISS, go says %+v", label, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("%s: python says %+v, go says MISS", label, *want)
			continue
		}
		if got.RuleID != want.RuleID || got.Fqdn1Pattern != want.Fqdn1Pattern ||
			got.Port != want.Port || got.Fqdn2Pattern != want.Fqdn2Pattern ||
			got.Rank1 != want.Rank1 || got.Rank2 != want.Rank2 {
			t.Errorf("%s:\n  python: %+v\n      go: %+v", label, *want, *got)
		}
	}
}

func describeQuery(i int, q goldenQuery) string {
	return fmt.Sprintf("query %d [%s] %s:%d -> %s", i, q.Mode, q.Fqdn1, q.Port, q.Fqdn2)
}

// The two stores hold the same rules under different namespaces, so after
// rewriting the namespace segment their physical key sets must be identical.
// Matching lookups alone would not catch a storage-layout divergence that both
// sides happen to read back consistently.
func assertSameKeyLayout(t *testing.T, ctx context.Context, conn *redis.Client) {
	t.Helper()
	py := namespaceKeys(t, ctx, conn, "xpy")
	goKeys := namespaceKeys(t, ctx, conn, "xgo")
	if len(py) == 0 {
		t.Fatalf("namespace xpy holds no keys; run crosscheck_python.py first")
	}
	if len(py) != len(goKeys) {
		t.Fatalf("key count differs: python %d, go %d\npython: %v\ngo:     %v", len(py), len(goKeys), py, goKeys)
	}
	for i := range py {
		if py[i] != goKeys[i] {
			t.Errorf("key %d differs: python %q, go %q", i, py[i], goKeys[i])
		}
	}
}

// A namespace spans two key shapes: the apex tier hashes the namespace itself
// (r:{ns:<tag>}:...) while the bucketed tiers hash only the slot token and
// carry the namespace as a plain prefix (r:<ns>:{<token>}:...). Neither scan
// pattern finds the other's keys, so both are needed.
func namespaceScanPatterns(ns string) []string {
	return []string{"r:{" + ns + ":*", "r:" + ns + ":*"}
}

func rawNamespaceKeys(t *testing.T, ctx context.Context, conn *redis.Client, ns string) []string {
	t.Helper()
	var all []string
	for _, pattern := range namespaceScanPatterns(ns) {
		keys, err := conn.Keys(ctx, pattern).Result()
		if err != nil {
			t.Fatalf("KEYS %s: %v", pattern, err)
		}
		all = append(all, keys...)
	}
	return all
}

func namespaceKeys(t *testing.T, ctx context.Context, conn *redis.Client, ns string) []string {
	t.Helper()
	keys := rawNamespaceKeys(t, ctx, conn, ns)
	normalized := make([]string, len(keys))
	for i, k := range keys {
		k = strings.Replace(k, "r:{"+ns+":", "r:{NS:", 1)
		normalized[i] = strings.Replace(k, "r:"+ns+":", "r:NS:", 1)
	}
	sort.Strings(normalized)
	return normalized
}

func clearNamespace(t *testing.T, ctx context.Context, conn *redis.Client, ns string) {
	t.Helper()
	keys := rawNamespaceKeys(t, ctx, conn, ns)
	if len(keys) > 0 {
		if err := conn.Del(ctx, keys...).Err(); err != nil {
			t.Fatalf("DEL for %s: %v", ns, err)
		}
	}
}
