// Redis/Dragonfly-backed 3-way (fqdn1, port, fqdn2) rule matcher.
//
// Storage: one Redis HASH per (fqdn1 pattern, port), hash-tagged so a lookup
// touching several ancestor-candidate keys stays inside a small, bounded
// number of Dragonfly/Redis-Cluster slots, never one slot per candidate:
//
//	r:{ns:<apex>}:<fqdn1_pattern>:<port>
//	r:<ns>:{<token>}:<tag>:<bucket_index>:<fqdn1_pattern>:<port>
//
// The apex tier (one key per registrable domain) is never bucketed. The
// __tld__ and __global__ tiers are a single logical key each for every query,
// so a RuleStore built with numBuckets > 1 replicates those two tiers across
// that many physical keys instead, written to all of them and read from one.
// <token> is a deterministic, CRC16-slot-targeted string from
// ComputeBucketTable(numBuckets) in slots.go -- the sole hashed content, so
// every replica of a given bucket index (across every tag family and
// namespace) lands on the same slot. numBuckets=1 keeps the __tld__/__global__
// tiers on the apex-style unbucketed key form, byte for byte.
//
// Each hash maps fqdn2_pattern to "<scope_char><rule_id>". fqdn2 is never a
// Redis key, so a lookup costs |fqdn1 candidates| keys probed, not
// |fqdn1 candidates| x |fqdn2 candidates|.
package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

const luaLookup = `
-- KEYS[1..n]: candidate rule-hash keys for one hash-tag group, ascending specificity.
-- ARGV[1]:    comma-joined needs_multi bits for KEYS, ascending specificity (fqdn1 side)
-- ARGV[2]:    comma-joined "is the exact rank-0 candidate" bits for KEYS, same order
-- ARGV[3]:    comma-joined needs_multi bits for fields, ascending specificity (fqdn2 side)
-- ARGV[4..]:  field names (fqdn2 candidate patterns), ascending specificity -- fields[1] is
--              always the exact rank-0 fqdn2 candidate, never regrouped, so no separate
--              "is exact" bit list is needed for the field side.
local function bits(s)
    local t = {}
    for b in string.gmatch(s, "[^,]+") do
        t[#t + 1] = (b == "1")
    end
    return t
end

local kmulti = bits(ARGV[1])
local kexact = bits(ARGV[2])
local fmulti = bits(ARGV[3])
local fields = {}
for i = 4, #ARGV do
    fields[#fields + 1] = ARGV[i]
end

for i = 1, #KEYS do
    local vals = redis.call('HMGET', KEYS[i], unpack(fields))
    for j = 1, #vals do
        local v = vals[j]
        if v then
            -- A fully exact hit (both sides matched their literal, non-wildcard
            -- pattern) is never scope-filtered, regardless of the rule's own
            -- declared scope -- scope only restricts how far a rule's *wildcard*
            -- may reach, and an exact match involves no wildcard on either side.
            local exact = kexact[i] and (j == 1)
            local deep = kmulti[i] or fmulti[j]
            local scope = string.sub(v, 1, 1)
            local blocked = (not exact) and ((deep and scope == 'S') or ((not deep) and scope == 'M'))
            if not blocked then
                return { i, j, string.sub(v, 2) }
            end
        end
    end
end
return nil
`

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

type RuleStore struct {
	ctx        context.Context
	conn       *redis.Client
	namespace  string
	numBuckets int
	// The slot-targeted token table for numBuckets, resolved once at
	// construction. Nil exactly when this store does not bucket
	// (numBuckets <= 1), which is what key() branches on.
	bucketTable *BucketTable
	sha         string
}

// PoolStats exposes the underlying go-redis connection pool's counters so the
// query phase can check whether a slow query coincided with a new connection
// being established (Misses/StaleConns) rather than assuming SCRIPT LOAD's
// one-time cost at startup explains everything -- a mid-run reconnect (to
// this node or, on a managed/HA service, a different one) pays a fresh
// TLS+TCP handshake, and on an unprimed node would need the Lua script
// reloaded too. Satisfies the optional PoolStatsLookuper interface in
// lookuper.go; GRPCLookuper has no local Redis connection and doesn't.
func (s *RuleStore) PoolStats() *redis.PoolStats {
	return s.conn.PoolStats()
}

func validatePort(port int) error {
	if port <= 0 || port >= 65536 {
		return invalidFqdn("port %d out of range 1..65535", port)
	}
	return nil
}

func NewRuleStore(ctx context.Context, conn *redis.Client, namespace string, numBuckets int) (*RuleStore, error) {
	if numBuckets < 1 {
		numBuckets = 1
	}
	var table *BucketTable
	if numBuckets > 1 {
		var err error
		if table, err = ComputeBucketTable(numBuckets); err != nil {
			return nil, fmt.Errorf("resolving bucket tokens: %w", err)
		}
	}
	sha, err := conn.ScriptLoad(ctx, luaLookup).Result()
	if err != nil {
		return nil, fmt.Errorf("loading lookup script: %w", err)
	}
	s := &RuleStore{
		ctx: ctx, conn: conn, namespace: namespace,
		numBuckets: numBuckets, bucketTable: table, sha: sha,
	}

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
				"RuleStore asks for num_buckets=%d; rewrite the catch-all-tier rules under the "+
				"new count before querying, or use the count already on record",
			namespace, claimed, numBuckets)
	}
	return s, nil
}

// Physical key text for one candidate. A bucketed tier (tag is
// __global__/__tld__:<x> and this store actually replicates) hashes only the
// deterministic slot-targeted token for bucketIndex; namespace, tag,
// bucketIndex, pattern and port all stay outside the braces as plain key-body
// text, so a SCAN r:<ns>:* prefix scan still finds them. Anything else (the
// apex tier, or a bucketable tag when numBuckets <= 1) keeps the original
// unbucketed form, byte for byte.
func (s *RuleStore) key(tag string, bucketIndex int, fqdn1Pattern string, port int) string {
	if s.bucketTable != nil && isBucketedTag(tag) {
		return fmt.Sprintf("r:%s:{%s}:%s:%d:%s:%d",
			s.namespace, s.bucketTable.Token(bucketIndex), tag, bucketIndex, fqdn1Pattern, port)
	}
	return fmt.Sprintf("r:{%s:%s}:%s:%d", s.namespace, tag, fqdn1Pattern, port)
}

func (s *RuleStore) bucketingMetaKey() string {
	return fmt.Sprintf("r:{%s:__meta__}:bucketing", s.namespace)
}

// Physical key(s) a rule registered under pattern, or a lookup candidate
// matching it, maps to. An apex pattern maps to exactly one key. A global/tld
// pattern maps to numBuckets replica keys, so put/delete fan out to all of them
// while a lookup (whose candidate already picked one bucket) checks only one.
func (s *RuleStore) keysForPattern(pattern string, port int) []string {
	baseTag := hashTag(pattern)
	if isBucketedTag(baseTag) && s.bucketTable != nil {
		keys := make([]string, s.numBuckets)
		for i := range keys {
			keys[i] = s.key(baseTag, i, pattern, port)
		}
		return keys
	}
	return []string{s.key(baseTag, 0, pattern, port)}
}

func (s *RuleStore) Put(rule Rule) error {
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
func (s *RuleStore) PutMany(rules []Rule) error {
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
func (s *RuleStore) Delete(fqdn1 string, port int, fqdn2 string) (bool, error) {
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

// The best matching rule, or (nil, nil) on a clean miss. Declares every key up
// front (grouped by hash tag, at most 3 groups) and dispatches one EVALSHA per
// group inside a single pipeline, one network round trip. Each group returns its
// own most-specific hit; the overall winner is the minimum (Rank1, Rank2).
func (s *RuleStore) Lookup(fqdn1 string, port int, fqdn2 string, mode MatchMode) (*Match, error) {
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
	fields := make([]interface{}, 0, len(bCandidates))
	for _, c := range bCandidates {
		fields = append(fields, c.Pattern)
	}
	fmulti := fieldMultiBits(bCandidates)

	// Go maps have no iteration order, so first-seen tag order is kept explicitly.
	var tags []string
	groups := make(map[string][]Candidate)
	for _, c := range aCandidates {
		if _, seen := groups[c.Tag]; !seen {
			tags = append(tags, c.Tag)
		}
		groups[c.Tag] = append(groups[c.Tag], c)
	}

	pipe := s.conn.Pipeline()
	cmds := make([]*redis.Cmd, len(tags))
	for i, tag := range tags {
		group := groups[tag]
		keys := make([]string, len(group))
		for j, c := range group {
			keys[j] = s.key(tag, c.BucketIndex, c.Pattern, port)
		}
		args := append([]interface{}{multiBits(group), exactBits(group), fmulti}, fields...)
		cmds[i] = pipe.EvalSha(s.ctx, s.sha, keys, args...)
	}
	// A Lua nil from any command surfaces as redis.Nil on Exec, while every other
	// command in the pipeline still carries its own value and a nil Err().
	if _, err := pipe.Exec(s.ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("lookup pipeline: %w", err)
	}

	var best *Match
	for i, cmd := range cmds {
		if errors.Is(cmd.Err(), redis.Nil) {
			continue
		}
		if cmd.Err() != nil {
			return nil, fmt.Errorf("lookup group %q: %w", tags[i], cmd.Err())
		}
		hit, err := parseHit(cmd.Val())
		if err != nil {
			return nil, fmt.Errorf("lookup group %q: %w", tags[i], err)
		}
		if hit == nil {
			continue
		}
		group := groups[tags[i]]
		if hit.keyIndex < 1 || hit.keyIndex > len(group) {
			return nil, fmt.Errorf("lookup group %q: key index %d out of range 1..%d", tags[i], hit.keyIndex, len(group))
		}
		if hit.fieldIndex < 1 || hit.fieldIndex > len(bCandidates) {
			return nil, fmt.Errorf("lookup group %q: field index %d out of range 1..%d", tags[i], hit.fieldIndex, len(bCandidates))
		}
		aCand := group[hit.keyIndex-1]
		bCand := bCandidates[hit.fieldIndex-1]
		if best != nil && !moreSpecific(aCand.Rank, bCand.Rank, best.Rank1, best.Rank2) {
			continue
		}
		best = &Match{
			RuleID:       hit.ruleID,
			Fqdn1Pattern: aCand.Pattern,
			Port:         port,
			Fqdn2Pattern: bCand.Pattern,
			Rank1:        aCand.Rank,
			Rank2:        bCand.Rank,
		}
	}
	return best, nil
}

func moreSpecific(rank1, rank2, bestRank1, bestRank2 int) bool {
	if rank1 != bestRank1 {
		return rank1 < bestRank1
	}
	return rank2 < bestRank2
}

func multiBits(cs []Candidate) string {
	bits := make([]string, len(cs))
	for i, c := range cs {
		if c.NeedsMulti {
			bits[i] = "1"
		} else {
			bits[i] = "0"
		}
	}
	return strings.Join(bits, ",")
}

func exactBits(cs []Candidate) string {
	bits := make([]string, len(cs))
	for i, c := range cs {
		if c.Rank == 0 {
			bits[i] = "1"
		} else {
			bits[i] = "0"
		}
	}
	return strings.Join(bits, ",")
}

func fieldMultiBits(cs []FieldCandidate) string {
	bits := make([]string, len(cs))
	for i, c := range cs {
		if c.NeedsMulti {
			bits[i] = "1"
		} else {
			bits[i] = "0"
		}
	}
	return strings.Join(bits, ",")
}

type luaHit struct {
	keyIndex   int
	fieldIndex int
	ruleID     string
}

func parseHit(val interface{}) (*luaHit, error) {
	if val == nil {
		return nil, nil
	}
	parts, ok := val.([]interface{})
	if !ok {
		return nil, fmt.Errorf("script returned %T, want a 3-element array", val)
	}
	if len(parts) != 3 {
		return nil, fmt.Errorf("script returned %d elements, want 3", len(parts))
	}
	keyIndex, ok := parts[0].(int64)
	if !ok {
		return nil, fmt.Errorf("script returned key index of type %T, want int64", parts[0])
	}
	fieldIndex, ok := parts[1].(int64)
	if !ok {
		return nil, fmt.Errorf("script returned field index of type %T, want int64", parts[1])
	}
	ruleID, ok := parts[2].(string)
	if !ok {
		return nil, fmt.Errorf("script returned rule id of type %T, want string", parts[2])
	}
	return &luaHit{keyIndex: int(keyIndex), fieldIndex: int(fieldIndex), ruleID: ruleID}, nil
}

// One-time sizing helper reading the server's own reported thread count
// (Dragonfly's per-process shard count). Deliberately not called from
// NewRuleStore: picking numBuckets is a deployment-time decision that should
// stay fixed until a human rewrites the catch-all tier, not be silently
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
