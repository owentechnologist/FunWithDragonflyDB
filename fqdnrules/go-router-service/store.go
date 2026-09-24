// Redis/Dragonfly-backed 3-way (fqdn1, port, fqdn2) rule matcher — no server-side
// Lua. This is a fork of go-benchmark's router.go: the physical key layout, the
// write path (Put/PutMany/Delete), and the hash-tag scheme are byte-identical,
// so this store reads data go-benchmark's RuleStore already wrote. Only Lookup
// differs: instead of one EVALSHA per hash-tag group, it issues one HMGET per
// fqdn1 candidate key, all pipelined into a single round trip, and does the
// scope-filtering and specificity ranking in Go instead of inside Dragonfly.
//
//	r:{ns:<apex>}:<fqdn1_pattern>:<port>
//	r:{ns:__tld__:<tld>}:<fqdn1_pattern>:<port>
//	r:{ns:__global__}:<fqdn1_pattern>:<port>
package router

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

type Rule struct {
	Fqdn1  string // exact fqdn or a "*."-prefixed / bare "*" wildcard pattern
	Port   int
	Fqdn2  string
	RuleID string
	Scope  WildcardScope
}

type Match struct {
	RuleID       string
	Fqdn1Pattern string
	Port         int
	Fqdn2Pattern string
	Rank1        int // lower is more specific
	Rank2        int
}

// NoScriptRuleStore is store.go's counterpart to go-benchmark's RuleStore: same
// key layout and write path, but Lookup never loads or evaluates a Lua script.
type NoScriptRuleStore struct {
	ctx        context.Context
	conn       *redis.Client
	namespace  string
	numBuckets int
}

func validatePort(port int) error {
	if port <= 0 || port >= 65536 {
		return invalidFqdn("port %d out of range 1..65535", port)
	}
	return nil
}

func NewNoScriptRuleStore(ctx context.Context, conn *redis.Client, namespace string, numBuckets int) (*NoScriptRuleStore, error) {
	if numBuckets < 1 {
		numBuckets = 1
	}
	s := &NoScriptRuleStore{ctx: ctx, conn: conn, namespace: namespace, numBuckets: numBuckets}

	// Checked unconditionally, including numBuckets=1: a reader left at the
	// default against a namespace some other writer already bucketed would
	// otherwise silently miss every catch-all-tier rule instead of failing
	// loudly. Read only, so a pure reader cannot pin a namespace no writer has
	// populated; only Put/PutMany write the count.
	raw, err := conn.HGet(ctx, s.bucketingMetaKey(), "num_buckets").Result()
	if errors.Is(err, redis.Nil) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading bucketing metadata: %w", err)
	}
	claimed, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing recorded num_buckets %q: %w", raw, err)
	}
	if claimed != numBuckets {
		return nil, fmt.Errorf(
			"namespace %q holds catch-all-tier data written with num_buckets=%d, but this "+
				"NoScriptRuleStore asks for num_buckets=%d; rewrite the catch-all-tier rules under the "+
				"new count before querying, or use the count already on record",
			namespace, claimed, numBuckets)
	}
	return s, nil
}

func (s *NoScriptRuleStore) key(tag, fqdn1Pattern string, port int) string {
	return fmt.Sprintf("r:{%s:%s}:%s:%d", s.namespace, tag, fqdn1Pattern, port)
}

func (s *NoScriptRuleStore) bucketingMetaKey() string {
	return fmt.Sprintf("r:{%s:__meta__}:bucketing", s.namespace)
}

// Physical key(s) a rule registered under pattern, or a lookup candidate
// matching it, maps to. An apex pattern maps to exactly one key. A global/tld
// pattern maps to numBuckets replica keys, so put/delete fan out to all of them
// while a lookup (whose candidate already picked one bucket) checks only one.
func (s *NoScriptRuleStore) keysForPattern(pattern string, port int) []string {
	baseTag := hashTag(pattern)
	if isBucketedTag(baseTag) && s.numBuckets > 1 {
		keys := make([]string, s.numBuckets)
		for i := range keys {
			keys[i] = s.key(bucketedTag(baseTag, s.numBuckets, i), pattern, port)
		}
		return keys
	}
	return []string{s.key(baseTag, pattern, port)}
}

func (s *NoScriptRuleStore) Put(rule Rule) error {
	if err := validatePort(rule.Port); err != nil {
		return err
	}
	keys := s.keysForPattern(rule.Fqdn1, rule.Port)
	value := string(rule.Scope.storageByte()) + rule.RuleID
	if len(keys) == 1 {
		if err := s.conn.HSet(s.ctx, keys[0], rule.Fqdn2, value).Err(); err != nil {
			return fmt.Errorf("writing rule %q: %w", rule.RuleID, err)
		}
		return nil
	}
	pipe := s.conn.Pipeline()
	for _, key := range keys {
		pipe.HSet(s.ctx, key, rule.Fqdn2, value)
	}
	pipe.HSet(s.ctx, s.bucketingMetaKey(), "num_buckets", strconv.Itoa(s.numBuckets))
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("writing rule %q: %w", rule.RuleID, err)
	}
	return nil
}

// One pipeline for a bulk load. Order-independent and idempotent, so resuming a
// half-applied load converges to the same state.
func (s *NoScriptRuleStore) PutMany(rules []Rule) error {
	pipe := s.conn.Pipeline()
	bucketed := false
	for _, rule := range rules {
		if err := validatePort(rule.Port); err != nil {
			return err
		}
		keys := s.keysForPattern(rule.Fqdn1, rule.Port)
		if len(keys) > 1 {
			bucketed = true
		}
		value := string(rule.Scope.storageByte()) + rule.RuleID
		for _, key := range keys {
			pipe.HSet(s.ctx, key, rule.Fqdn2, value)
		}
	}
	if bucketed {
		pipe.HSet(s.ctx, s.bucketingMetaKey(), "num_buckets", strconv.Itoa(s.numBuckets))
	}
	if _, err := pipe.Exec(s.ctx); err != nil {
		return fmt.Errorf("writing %d rules: %w", len(rules), err)
	}
	return nil
}

// Redis/Dragonfly drop the hash when its last field goes, so key cleanup is
// automatic and a second call returns false. A bucketed pattern is cleared from
// every replica, and the result is true if the field existed in any.
func (s *NoScriptRuleStore) Delete(fqdn1 string, port int, fqdn2 string) (bool, error) {
	keys := s.keysForPattern(fqdn1, port)
	if len(keys) == 1 {
		n, err := s.conn.HDel(s.ctx, keys[0], fqdn2).Result()
		if err != nil {
			return false, fmt.Errorf("deleting rule: %w", err)
		}
		return n > 0, nil
	}
	pipe := s.conn.Pipeline()
	cmds := make([]*redis.IntCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.HDel(s.ctx, key, fqdn2)
	}
	if _, err := pipe.Exec(s.ctx); err != nil {
		return false, fmt.Errorf("deleting rule: %w", err)
	}
	deleted := false
	for _, cmd := range cmds {
		if cmd.Val() > 0 {
			deleted = true
		}
	}
	return deleted, nil
}

// The best matching rule, or (nil, nil) on a clean miss. Declares every fqdn1
// candidate's key up front and dispatches one HMGET per key inside a single
// pipeline: one network round trip, no Lua. Unlike a Lua EVALSHA, a pipelined
// HMGET can't short-circuit before the round trip returns, so every candidate
// key's full field list comes back regardless of where the winner turns out to
// be — the trade this architecture makes for having no server-side script.
//
// candidates() already returns both aCandidates and bCandidates in ascending
// (most-specific-first) rank order, so scanning the flattened aCandidates x
// bCandidates space in that order and returning the first unblocked hit is
// exactly the (rank1, rank2) minimum: fixing rank1 by trying keys in ascending
// rank order and only breaking ties on rank2 within the first key that has any
// unblocked hit. That's the same semantics as the Lua script's per-group loop,
// collapsed into one flat loop since there's no longer a per-EVALSHA slot
// boundary forcing candidates to be grouped by hash tag first.
func (s *NoScriptRuleStore) Lookup(fqdn1 string, port int, fqdn2 string, mode MatchMode) (*Match, error) {
	normalized1, err := normalizeFqdn(fqdn1)
	if err != nil {
		return nil, err
	}
	normalized2, err := normalizeFqdn(fqdn2)
	if err != nil {
		return nil, err
	}
	if err := validatePort(port); err != nil {
		return nil, err
	}

	aCandidates := candidates(normalized1, mode, s.numBuckets)
	bCandidates := fieldCandidates(normalized2, mode)
	fields := make([]string, len(bCandidates))
	for i, c := range bCandidates {
		fields[i] = c.Pattern
	}

	pipe := s.conn.Pipeline()
	cmds := make([]*redis.SliceCmd, len(aCandidates))
	for i, a := range aCandidates {
		key := s.key(a.Tag, a.Pattern, port)
		cmds[i] = pipe.HMGet(s.ctx, key, fields...)
	}
	if _, err := pipe.Exec(s.ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("lookup pipeline: %w", err)
	}

	for i, a := range aCandidates {
		vals, err := cmds[i].Result()
		if err != nil {
			return nil, fmt.Errorf("lookup key %q: %w", s.key(a.Tag, a.Pattern, port), err)
		}
		for j, b := range bCandidates {
			raw, ok := vals[j].(string)
			if !ok {
				continue // nil field: no rule registered for this (fqdn1, fqdn2) candidate pair
			}
			// A fully exact hit is never scope-filtered, regardless of the rule's own
			// declared scope -- scope only restricts how far a rule's wildcard may
			// reach, and an exact match involves no wildcard on either side.
			exact := a.Rank == 0 && b.Rank == 0
			deep := a.NeedsMulti || b.NeedsMulti
			scope := raw[0]
			blocked := !exact && ((deep && scope == byte(ScopeSingle)) || (!deep && scope == byte(ScopeMulti)))
			if blocked {
				continue
			}
			return &Match{
				RuleID:       raw[1:],
				Fqdn1Pattern: a.Pattern,
				Port:         port,
				Fqdn2Pattern: b.Pattern,
				Rank1:        a.Rank,
				Rank2:        b.Rank,
			}, nil
		}
	}
	return nil, nil
}

// One-time sizing helper reading the server's own reported thread count
// (Dragonfly's per-process shard count). Deliberately not called from
// NewNoScriptRuleStore: picking numBuckets is a deployment-time decision that
// should stay fixed until a human rewrites the catch-all tier, not be silently
// re-derived on every process restart.
func RecommendedNumBuckets(ctx context.Context, conn *redis.Client, def int) int {
	info, err := conn.Info(ctx, "server").Result()
	if err != nil {
		return def
	}
	for _, line := range strings.Split(info, "\n") {
		rest, found := strings.CutPrefix(strings.TrimSpace(line), "thread_count:")
		if !found {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return def
		}
		return n
	}
	return def
}
