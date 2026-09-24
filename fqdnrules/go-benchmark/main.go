// CLI benchmark for RuleStore (router.go): write N rules, run M timed lookups
// against them, and report write throughput plus lookup latency percentiles.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var benchPorts = [3]int{443, 8443, 80}

// Every key's fqdn1 apex is one of numDomains distinct domains, so a run spreads
// keys across numDomains Dragonfly hash tags instead of piling every key onto
// one tag, which would pin all writes and lookups to a single shard.
const numDomains = 10000

type Query struct {
	Fqdn1 string
	Port  int
	Fqdn2 string
}

type config struct {
	numRules         int
	numQueries       int
	host             string
	port             int
	db               int
	uri              string
	mode             string
	missRate         float64
	writeBatchSize   int
	ruleDepthTiers   string
	globalRuleRate   float64
	tldRuleRate      float64
	numBuckets       int
	seed             int64
	noFlush          bool
	queryConcurrency int
	target           string
	slowLatencyGate  float64
}

func parseFlags(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	fs.IntVar(&cfg.numRules, "num-rules", 100000, "number of rules to write")
	fs.IntVar(&cfg.numQueries, "num-queries", 10000, "number of lookups to time")
	fs.StringVar(&cfg.host, "host", "localhost", "redis/dragonfly host")
	fs.IntVar(&cfg.port, "port", 6379, "redis/dragonfly port")
	fs.IntVar(&cfg.db, "db", 0, "redis/dragonfly database index")
	fs.StringVar(&cfg.uri, "uri", "", "redis:// or rediss:// URI, an alternative to -host/-port/-db")
	fs.StringVar(&cfg.mode, "mode", "multi", "match mode, single or multi")
	fs.Float64Var(&cfg.missRate, "miss-rate", 0.1, "fraction of queries that should miss")
	fs.IntVar(&cfg.writeBatchSize, "write-batch-size", 5000, "rules per pipelined write batch")
	fs.StringVar(&cfg.ruleDepthTiers, "rule-depth-tiers", defaultDepthTiers,
		"comma-separated PCT:LO-HI tiers controlling how many rules get nested under one standard-zone key")
	fs.Float64Var(&cfg.globalRuleRate, "global-rule-rate", 0.05,
		"fraction of rules registered as global-tier (fqdn1=\"*\") catch-alls")
	fs.Float64Var(&cfg.tldRuleRate, "tld-rule-rate", 0.10,
		"fraction of rules registered as per-TLD (fqdn1=\"*.<tld>\") catch-alls")
	fs.IntVar(&cfg.numBuckets, "num-buckets", 16,
		"replicas for the global/tld catch-all tiers, so every lookup's check of those tiers spreads across this many keys")
	fs.Int64Var(&cfg.seed, "seed", 42, "seed for query selection and shuffling")
	fs.BoolVar(&cfg.noFlush, "no-flush", false, "skip the flush and write phase, query existing data")
	fs.IntVar(&cfg.queryConcurrency, "query-concurrency", 1, "goroutines issuing queries concurrently")
	fs.StringVar(&cfg.target, "target", "direct",
		"where the query phase sends lookups: \"direct\" (this process talks to Dragonfly itself, arch 2) "+
			"or \"grpc=host:port[,host:port,...]\" (round-robin across one or more go-router-service routerd "+
			"instances, arch 3). The write phase always writes directly, regardless of -target.")
	fs.Float64Var(&cfg.slowLatencyGate, "slow-latency-gate", 0.0,
		"minimum latency in ms for a query to be eligible for the top-slowest report; queries faster than "+
			"this are never recorded there, so the report can end up empty when nothing was that slow")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	return cfg, validateArgs(cfg)
}

func validateArgs(cfg config) error {
	if cfg.numRules < 1000 || cfg.numRules > 500000000 {
		return fmt.Errorf("-num-rules must be within [1000, 500000000], got %d", cfg.numRules)
	}
	if cfg.numQueries < 100 || cfg.numQueries > 50000000 {
		return fmt.Errorf("-num-queries must be within [100, 50000000], got %d", cfg.numQueries)
	}
	if cfg.missRate < 0.0 || cfg.missRate > 1.0 {
		return fmt.Errorf("-miss-rate must be within [0.0, 1.0], got %v", cfg.missRate)
	}
	if cfg.queryConcurrency < 1 || cfg.queryConcurrency > 256 {
		return fmt.Errorf("-query-concurrency must be within [1, 256], got %d", cfg.queryConcurrency)
	}
	if cfg.numBuckets < 1 {
		return fmt.Errorf("-num-buckets must be >= 1, got %d", cfg.numBuckets)
	}
	if cfg.writeBatchSize < 1 {
		return fmt.Errorf("-write-batch-size must be >= 1, got %d", cfg.writeBatchSize)
	}
	if cfg.slowLatencyGate < 0.0 {
		return fmt.Errorf("-slow-latency-gate must be >= 0, got %v", cfg.slowLatencyGate)
	}
	if cfg.mode != "single" && cfg.mode != "multi" {
		return fmt.Errorf("-mode must be single or multi, got %q", cfg.mode)
	}
	if _, err := parseDepthTiers(cfg.ruleDepthTiers); err != nil {
		return fmt.Errorf("-rule-depth-tiers: %w", err)
	}
	if err := validateRuleMixRates(cfg.globalRuleRate, cfg.tldRuleRate); err != nil {
		return err
	}
	if _, err := parseTarget(cfg.target); err != nil {
		return err
	}
	return nil
}

// parseTarget returns nil for "direct" or the routerd addresses for
// "grpc=host:port[,host:port,...]".
func parseTarget(target string) ([]string, error) {
	if target == "direct" {
		return nil, nil
	}
	addrList, ok := strings.CutPrefix(target, "grpc=")
	if !ok {
		return nil, fmt.Errorf(`-target must be "direct" or "grpc=host:port[,host:port,...]", got %q`, target)
	}
	addrs := strings.Split(addrList, ",")
	for i, a := range addrs {
		addrs[i] = strings.TrimSpace(a)
		if addrs[i] == "" {
			return nil, fmt.Errorf("-target %q has an empty routerd address", target)
		}
	}
	return addrs, nil
}

func domainForKey(keyID int) string {
	return fmt.Sprintf("dragonfly%d.com", keyID%numDomains)
}

// The fqdn1 pattern, hash-tag apex, and port are all properties of the key,
// shared by every rule nested under it.
func keyFields(keyID int) (int, string, int) {
	return keyID % 1000, domainForKey(keyID), benchPorts[keyID%3]
}

func ruleForIndex(i int, mix *RuleMix) Rule {
	var fqdn1 string
	var port int
	tier, tierIndex := mix.Tier(i)
	switch tier {
	case TierGlobal:
		fqdn1, port = "*", benchPorts[i%len(benchPorts)]
	case TierTld:
		fqdn1, port = "*."+syntheticTld(tierIndex), benchPorts[i%len(benchPorts)]
	case TierApex:
		keyID, _ := mix.layout.KeyForRule(tierIndex)
		bucket, domain, keyPort := keyFields(keyID)
		fqdn1, port = fmt.Sprintf("*.host%d.corp%d.%s", keyID, bucket, domain), keyPort
	}
	return Rule{
		Fqdn1:  fqdn1,
		Port:   port,
		Fqdn2:  fmt.Sprintf("*.dest%d.example.com", i),
		RuleID: fmt.Sprintf("rule:%d", i),
	}
}

// The global-tier probe's ".invalid" TLD and "global-bench" label are never
// registered by any tier, so only the "*" rule can match it. The TLD-tier probe
// is exactly two labels, so its "*.<tld>" candidate is generated under both
// SingleLevel and MultiLevel.
func matchingQueryForIndex(i int, mix *RuleMix) Query {
	var fqdn1 string
	var port int
	tier, tierIndex := mix.Tier(i)
	switch tier {
	case TierGlobal:
		fqdn1, port = fmt.Sprintf("probe%d.global-bench.invalid", i), benchPorts[i%len(benchPorts)]
	case TierTld:
		fqdn1, port = fmt.Sprintf("probe%d.%s", i, syntheticTld(tierIndex)), benchPorts[i%len(benchPorts)]
	case TierApex:
		keyID, _ := mix.layout.KeyForRule(tierIndex)
		bucket, domain, keyPort := keyFields(keyID)
		fqdn1, port = fmt.Sprintf("probe.host%d.corp%d.%s", keyID, bucket, domain), keyPort
	}
	return Query{
		Fqdn1: fqdn1,
		Port:  port,
		Fqdn2: fmt.Sprintf("probe.dest%d.example.com", i),
	}
}

// Each miss gets its own apex zone (junk<N>.invalid) rather than one fixed
// domain shared by every miss query. A shared apex would put every miss's
// wildcard candidate on the same unbucketed key, hammering it with 100% of
// miss traffic regardless of concurrency -- the fix mirrors how real
// apex-tier domains already shard naturally, one key per domain.
func missQuery(rng *rand.Rand) Query {
	n := rng.Int63n(1 << 31)
	junkApex := rng.Int63n(1 << 31)
	return Query{
		Fqdn1: fmt.Sprintf("nomatch%d.junk%d.invalid", n, junkApex),
		Port:  443,
		Fqdn2: fmt.Sprintf("nomatch%d.invalid", n),
	}
}

func humanBytes(n int64) string {
	value := float64(n)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if value < 1024.0 {
			if unit == "B" {
				return fmt.Sprintf("%d %s", int64(value), unit)
			}
			return fmt.Sprintf("%.2f %s", value, unit)
		}
		value /= 1024.0
	}
	return fmt.Sprintf("%.2f TB", value)
}

type LatencyStats struct {
	Min  float64
	Avg  float64
	P50  float64
	P90  float64
	P99  float64
	P999 float64
	Max  float64
}

func computeLatencyStats(latencies []time.Duration) LatencyStats {
	sorted := make([]float64, len(latencies))
	for i, d := range latencies {
		sorted[i] = float64(d) / float64(time.Millisecond)
	}
	sort.Float64s(sorted)
	n := len(sorted)

	nearestRank := func(p float64) float64 {
		idx := int(math.Floor(p * float64(n)))
		if idx > n-1 {
			idx = n - 1
		}
		return sorted[idx]
	}

	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	return LatencyStats{
		Min:  sorted[0],
		Avg:  sum / float64(n),
		P50:  nearestRank(0.50),
		P90:  nearestRank(0.90),
		P99:  nearestRank(0.99),
		P999: nearestRank(0.999),
		Max:  sorted[n-1],
	}
}

type SampleResult struct {
	Index     int
	Fqdn1     string
	Port      int
	Fqdn2     string
	Result    string
	LatencyMs float64
}

const topSlowLimit = 10

type TopSlowResult struct {
	Timestamp time.Time
	Fqdn1     string
	Port      int
	Fqdn2     string
	Result    string
	LatencyMs float64

	// Pool stat deltas captured immediately before/after this query's
	// store.Lookup call, only populated when the store implements
	// PoolStatsLookuper (arch 2/direct; not available over gRPC to routerd,
	// which owns its own separate pool to Dragonfly). A nonzero
	// PoolMissesDelta or PoolStaleConnsDelta means a new connection was
	// established during roughly this query's execution window -- evidence
	// for the "reconnect mid-run pays a fresh TLS handshake" theory as an
	// explanation for a slow query, distinct from anything server-side.
	PoolStatsAvailable  bool
	PoolHitsDelta       uint32
	PoolMissesDelta     uint32
	PoolTimeoutsDelta   uint32
	PoolStaleConnsDelta uint32
}

// Keeps list sorted descending by LatencyMs, capped at topSlowLimit. Called
// once per query on a per-worker slice (no shared lock needed, same pattern
// perWorkerSamples already uses), then all workers' slices are merged and
// re-truncated after the timed phase.
func insertTopSlow(list []TopSlowResult, entry TopSlowResult) []TopSlowResult {
	if len(list) < topSlowLimit {
		list = append(list, entry)
		sort.Slice(list, func(i, j int) bool { return list[i].LatencyMs > list[j].LatencyMs })
		return list
	}
	if entry.LatencyMs <= list[len(list)-1].LatencyMs {
		return list
	}
	list[len(list)-1] = entry
	sort.Slice(list, func(i, j int) bool { return list[i].LatencyMs > list[j].LatencyMs })
	return list
}

func printLayoutReport(mix *RuleMix, tiers []DepthTier, numRules int) {
	layout := mix.layout
	// -global-rule-rate plus -tld-rule-rate may legally sum to 1.0, leaving zero
	// apex rules and an empty layout, so KeySizes[0] is not always safe to index.
	minSize, maxSize := 0, 0
	if len(layout.KeySizes) > 0 {
		minSize, maxSize = layout.KeySizes[0], layout.KeySizes[0]
		for _, s := range layout.KeySizes {
			if s < minSize {
				minSize = s
			}
			if s > maxSize {
				maxSize = s
			}
		}
	}
	rulesPerKey := 0.0
	if layout.NumKeys() > 0 {
		rulesPerKey = float64(mix.NumApex()) / float64(layout.NumKeys())
	}
	fmt.Println()
	fmt.Println("=== Key layout ===")
	fmt.Printf("rules total           : %s\n", comma(int64(numRules)))
	fmt.Printf("  global-tier rules   : %s (%.1f%%)\n",
		comma(int64(mix.NumGlobal())), float64(mix.NumGlobal())/float64(numRules)*100)
	fmt.Printf("  tld-tier rules      : %s (%.1f%%)\n",
		comma(int64(mix.NumTld())), float64(mix.NumTld())/float64(numRules)*100)
	fmt.Printf("  apex-tier rules     : %s (%.1f%%)\n",
		comma(int64(mix.NumApex())), float64(mix.NumApex())/float64(numRules)*100)
	fmt.Printf("standard zones (keys) : %s\n", comma(int64(layout.NumKeys())))
	fmt.Printf("rules per key         : avg %.1f, min %d, max %d\n", rulesPerKey, minSize, maxSize)
	for idx, tier := range tiers {
		keysInTier, rulesInTier := 0, 0
		for _, s := range layout.KeySizes {
			if s >= tier.MinSize && s <= tier.MaxSize {
				keysInTier++
				rulesInTier += s
			}
		}
		fmt.Printf("  tier %d [%d-%d] (target %.1f%%): %s keys, %s rules\n",
			idx+1, tier.MinSize, tier.MaxSize, tier.Weight*100,
			comma(int64(keysInTier)), comma(int64(rulesInTier)))
	}
}

func runWritePhase(store *RuleStore, numRules, batchSize int, mix *RuleMix) (int, int64, time.Duration, error) {
	numBatches := (numRules + batchSize - 1) / batchSize
	progressInterval := numBatches / 100
	if progressInterval < 20 {
		progressInterval = 20
	}

	totalRules := 0
	var totalBytes int64
	start := time.Now()
	for batchIdx := 0; batchIdx < numBatches; batchIdx++ {
		lo := batchIdx * batchSize
		hi := lo + batchSize
		if hi > numRules {
			hi = numRules
		}
		batch := make([]Rule, 0, hi-lo)
		for i := lo; i < hi; i++ {
			batch = append(batch, ruleForIndex(i, mix))
		}
		if err := store.PutMany(batch); err != nil {
			return totalRules, totalBytes, time.Since(start), err
		}

		totalRules += len(batch)
		for _, r := range batch {
			totalBytes += int64(len(r.Fqdn2) + len(r.RuleID) + 1)
		}

		isLast := batchIdx+1 == numBatches
		if isLast || (batchIdx+1)%progressInterval == 0 {
			fmt.Printf("  write progress: %s/%s rules, %s written, %.1fs elapsed\n",
				comma(int64(totalRules)), comma(int64(numRules)), humanBytes(totalBytes), time.Since(start).Seconds())
		}
	}
	return totalRules, totalBytes, time.Since(start), nil
}

func buildQueries(numRules, numQueries int, missRate float64, rng *rand.Rand, mix *RuleMix) []Query {
	numHits := int(math.Round(float64(numQueries) * (1 - missRate)))
	numMisses := numQueries - numHits
	queries := make([]Query, 0, numQueries)
	for i := 0; i < numHits; i++ {
		queries = append(queries, matchingQueryForIndex(rng.Intn(numRules), mix))
	}
	for i := 0; i < numMisses; i++ {
		queries = append(queries, missQuery(rng))
	}
	rng.Shuffle(len(queries), func(i, j int) { queries[i], queries[j] = queries[j], queries[i] })
	return queries
}

// go-redis dials lazily, so without this the first query on each pooled
// connection would pay its connect (and, for rediss://, TLS and auth) cost
// inside that query's measured latency. The WaitGroup barrier holds every
// goroutine until all of them are running, so each one draws a distinct
// connection rather than one fast goroutine absorbing every warm-up lookup.
func warmUpConnections(store Lookuper, mode MatchMode, concurrency int, rng *rand.Rand) {
	if concurrency <= 1 {
		return
	}
	warmQueries := make([]Query, concurrency)
	for i := range warmQueries {
		warmQueries[i] = missQuery(rng)
	}

	var barrier, done sync.WaitGroup
	barrier.Add(concurrency)
	done.Add(concurrency)
	for _, q := range warmQueries {
		go func(q Query) {
			defer done.Done()
			barrier.Done()
			barrier.Wait()
			store.Lookup(q.Fqdn1, q.Port, q.Fqdn2, mode)
		}(q)
	}
	done.Wait()
}

// Returns per-query latencies in submission order, the sampled rows, the top
// topSlowLimit slowest queries at or above slowLatencyGate ms (timestamp,
// fqdn1/port/fqdn2, and result), the wall-clock time for the whole phase, and
// how many lookups returned an error. The error count is reported because a
// run whose every lookup failed would otherwise post an excellent
// queries/sec figure and look like a success.
func runQueryPhase(store Lookuper, queries []Query, mode MatchMode, concurrency int, slowLatencyGate float64) ([]time.Duration, []SampleResult, []TopSlowResult, time.Duration, int) {
	numQueries := len(queries)
	sampleIndices := make(map[int]bool, 10)
	for i := 0; i < 10; i++ {
		sampleIndices[int(math.Round(float64(i)*float64(numQueries-1)/9.0))] = true
	}

	latencies := make([]time.Duration, numQueries)
	perWorkerSamples := make([][]SampleResult, concurrency)
	perWorkerTopSlow := make([][]TopSlowResult, concurrency)
	perWorkerErrors := make([]int, concurrency)
	firstError := make([]error, concurrency)
	indices := make(chan int, concurrency*2)

	// PoolStats() is just an atomic-counter read (no network call), so
	// bracketing every query with one is cheap even at millions of
	// queries/sec. Only *RuleStore (direct target) implements this -- there's
	// nothing to bracket when talking to routerd over gRPC, since routerd
	// owns its own separate pool to Dragonfly that this process can't see.
	psl, hasPoolStats := store.(PoolStatsLookuper)

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for w := 0; w < concurrency; w++ {
		go func(worker int) {
			defer wg.Done()
			for idx := range indices {
				q := queries[idx]
				var before *redis.PoolStats
				if hasPoolStats {
					before = psl.PoolStats()
				}
				t0 := time.Now()
				match, err := store.Lookup(q.Fqdn1, q.Port, q.Fqdn2, mode)
				latencies[idx] = time.Since(t0)
				latencyMs := float64(latencies[idx]) / float64(time.Millisecond)
				if err != nil {
					perWorkerErrors[worker]++
					if firstError[worker] == nil {
						firstError[worker] = err
					}
				}
				result := "MISS"
				if err != nil {
					result = "ERROR"
				} else if match != nil {
					result = match.RuleID
				}
				if latencyMs >= slowLatencyGate {
					entry := TopSlowResult{
						Timestamp: t0, Fqdn1: q.Fqdn1, Port: q.Port, Fqdn2: q.Fqdn2,
						Result: result, LatencyMs: latencyMs,
					}
					if hasPoolStats {
						after := psl.PoolStats()
						entry.PoolStatsAvailable = true
						entry.PoolHitsDelta = after.Hits - before.Hits
						entry.PoolMissesDelta = after.Misses - before.Misses
						entry.PoolTimeoutsDelta = after.Timeouts - before.Timeouts
						entry.PoolStaleConnsDelta = after.StaleConns - before.StaleConns
					}
					perWorkerTopSlow[worker] = insertTopSlow(perWorkerTopSlow[worker], entry)
				}
				if !sampleIndices[idx] {
					continue
				}
				perWorkerSamples[worker] = append(perWorkerSamples[worker], SampleResult{
					Index: idx, Fqdn1: q.Fqdn1, Port: q.Port, Fqdn2: q.Fqdn2,
					Result: result, LatencyMs: latencyMs,
				})
			}
		}(w)
	}
	for idx := range queries {
		indices <- idx
	}
	close(indices)
	wg.Wait()
	elapsed := time.Since(start)

	var samples []SampleResult
	for _, s := range perWorkerSamples {
		samples = append(samples, s...)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].Index < samples[j].Index })

	var topSlow []TopSlowResult
	for _, s := range perWorkerTopSlow {
		topSlow = append(topSlow, s...)
	}
	sort.Slice(topSlow, func(i, j int) bool { return topSlow[i].LatencyMs > topSlow[j].LatencyMs })
	if len(topSlow) > topSlowLimit {
		topSlow = topSlow[:topSlowLimit]
	}

	numErrors := 0
	for _, n := range perWorkerErrors {
		numErrors += n
	}
	if numErrors > 0 {
		for _, err := range firstError {
			if err != nil {
				fmt.Printf("\nwarning: %s of %s lookups returned an error, first was: %v\n",
					comma(int64(numErrors)), comma(int64(numQueries)), err)
				break
			}
		}
	}
	return latencies, samples, topSlow, elapsed, numErrors
}

func printWriteReport(numRules int, numBytes int64, elapsed time.Duration) {
	fmt.Println()
	fmt.Println("=== Write phase ===")
	fmt.Printf("rules written : %s\n", comma(int64(numRules)))
	fmt.Printf("data written  : %s\n", humanBytes(numBytes))
	fmt.Printf("elapsed       : %.2fs\n", elapsed.Seconds())
	fmt.Printf("rate          : %s rules/sec\n", commaRate(float64(numRules), elapsed))
}

func printLatencyReport(stats LatencyStats, numQueries int, elapsed time.Duration, concurrency, numErrors int) {
	fmt.Println()
	fmt.Println("=== Query latency (ms) ===")
	fmt.Printf("%8s %8s %8s %8s %8s %8s %8s\n", "min", "avg", "p50", "p90", "p99", "p99.9", "max")
	fmt.Printf("%8.3f %8.3f %8.3f %8.3f %8.3f %8.3f %8.3f\n",
		stats.Min, stats.Avg, stats.P50, stats.P90, stats.P99, stats.P999, stats.Max)
	fmt.Println()
	fmt.Println("=== Query throughput ===")
	fmt.Printf("queries       : %s\n", comma(int64(numQueries)))
	fmt.Printf("errors        : %s\n", comma(int64(numErrors)))
	fmt.Printf("concurrency   : %d\n", concurrency)
	fmt.Printf("elapsed       : %.2fs\n", elapsed.Seconds())
	fmt.Printf("rate          : %s queries/sec\n", commaRate(float64(numQueries), elapsed))
}

func printSampleReport(samples []SampleResult) {
	fmt.Println()
	fmt.Println("=== Sample results (10 evenly-spaced queries) ===")
	fmt.Printf("%6s  %-45s %5s %-30s %-20s %10s\n", "idx", "fqdn1", "port", "fqdn2", "result", "latency_ms")
	for _, s := range samples {
		fmt.Printf("%6d  %-45s %5d %-30s %-20s %10.3f\n",
			s.Index, s.Fqdn1, s.Port, s.Fqdn2, s.Result, s.LatencyMs)
	}
}

// describeQueryOps reconstructs, for reporting only, the Redis-level
// operation and key names a Lookup(fqdn1, port, fqdn2, mode) call issues --
// purely by re-running the same candidates() logic Lookup itself uses, so it
// costs nothing during the timed phase and only runs for the handful of rows
// the top-slow report prints. namespace mirrors the "bench" constant Put/
// NewRuleStore write under; target selects EVALSHA-grouped-by-tag (direct,
// router.go's Lua path) vs one HMGET per candidate (grpc=..., routerd's
// no-Lua path).
func describeQueryOps(fqdn1 string, port int, mode MatchMode, numBuckets int, namespace, target string) (op string, keys []string) {
	normalized, err := normalizeFqdn(fqdn1)
	if err != nil {
		return "invalid fqdn1", nil
	}
	cands := candidates(normalized, mode, numBuckets)
	for _, c := range cands {
		keys = append(keys, fmt.Sprintf("r:{%s:%s}:%s:%d", namespace, c.Tag, c.Pattern, port))
	}
	if strings.HasPrefix(target, "grpc=") {
		return fmt.Sprintf("HMGET x%d", len(cands)), keys
	}
	seen := make(map[string]bool, len(cands))
	numGroups := 0
	for _, c := range cands {
		if !seen[c.Tag] {
			seen[c.Tag] = true
			numGroups++
		}
	}
	return fmt.Sprintf("EVALSHA x%d", numGroups), keys
}

func printTopSlowReport(entries []TopSlowResult, mode MatchMode, numBuckets int, target string, slowLatencyGate float64) {
	fmt.Println()
	fmt.Printf("=== Top %d slowest queries (>= %.3fms) ===\n", topSlowLimit, slowLatencyGate)
	if len(entries) == 0 {
		fmt.Printf("  (no queries at or above the %.3fms gate)\n", slowLatencyGate)
		return
	}
	for i, e := range entries {
		op, keys := describeQueryOps(e.Fqdn1, e.Port, mode, numBuckets, "bench", target)
		fmt.Printf("%2d. %s  %9.3fms  %-10s fqdn1=%s port=%d fqdn2=%s result=%s\n",
			i+1, e.Timestamp.Format("2006-01-02T15:04:05.000Z07:00"), e.LatencyMs, op,
			e.Fqdn1, e.Port, e.Fqdn2, e.Result)
		fmt.Printf("      keys: %s\n", strings.Join(keys, ", "))
		if e.PoolStatsAvailable {
			flag := ""
			if e.PoolMissesDelta > 0 || e.PoolStaleConnsDelta > 0 {
				flag = "  <-- NEW CONNECTION ESTABLISHED DURING THIS QUERY"
			}
			fmt.Printf("      pool during this query: hits=+%d misses=+%d timeouts=+%d staleConns=+%d%s\n",
				e.PoolHitsDelta, e.PoolMissesDelta, e.PoolTimeoutsDelta, e.PoolStaleConnsDelta, flag)
		}
	}
}

// printPoolStatsSummary prints the connection pool's cumulative counters over
// the whole run, for context against the per-query deltas in the top-slow
// report -- e.g. "18 misses total, 2 of them landed on top-10-slowest
// queries" is a much stronger signal than a miss count alone.
func printPoolStatsSummary(stats *redis.PoolStats) {
	if stats == nil {
		return
	}
	fmt.Println()
	fmt.Println("=== Connection pool stats (whole run) ===")
	fmt.Printf("hits=%d misses=%d timeouts=%d staleConns=%d totalConns=%d idleConns=%d\n",
		stats.Hits, stats.Misses, stats.Timeouts, stats.StaleConns, stats.TotalConns, stats.IdleConns)
}

func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	return sign + b.String()
}

func commaRate(count float64, elapsed time.Duration) string {
	if elapsed <= 0 {
		return "inf"
	}
	return comma(int64(math.Round(count / elapsed.Seconds())))
}

func describeTarget(cfg config) string {
	if cfg.uri == "" {
		return fmt.Sprintf("%s:%d db=%d", cfg.host, cfg.port, cfg.db)
	}
	u, err := url.Parse(cfg.uri)
	if err != nil || u.User == nil {
		return cfg.uri
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return cfg.uri
	}
	// url.UserPassword percent-encodes the mask into %2A%2A%2A, so the host
	// portion is rebuilt by hand to keep the printed target readable.
	return fmt.Sprintf("%s://%s:***@%s%s", u.Scheme, u.User.Username(), u.Host, u.Path)
}

func newClient(cfg config) (*redis.Client, error) {
	var opts *redis.Options
	if cfg.uri != "" {
		parsed, err := redis.ParseURL(cfg.uri)
		if err != nil {
			return nil, fmt.Errorf("parsing -uri: %w", err)
		}
		opts = parsed
	} else {
		opts = &redis.Options{Addr: fmt.Sprintf("%s:%d", cfg.host, cfg.port), DB: cfg.db}
	}
	// go-redis defaults PoolSize to 10*GOMAXPROCS, which can sit below a high
	// -query-concurrency and serialize the workers behind the pool.
	poolSize := 10 * runtime.GOMAXPROCS(0)
	if cfg.queryConcurrency > poolSize {
		poolSize = cfg.queryConcurrency
	}
	opts.PoolSize = poolSize
	return redis.NewClient(opts), nil
}

func fail(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	mode := MultiLevel
	if cfg.mode == "single" {
		mode = SingleLevel
	}

	ctx := context.Background()
	conn, err := newClient(cfg)
	if err != nil {
		fail("%v", err)
	}
	defer conn.Close()
	if err := conn.Ping(ctx).Err(); err != nil {
		fail("cannot reach redis at %s: %v", describeTarget(cfg), err)
	}

	store, err := NewRuleStore(ctx, conn, "bench", cfg.numBuckets)
	if err != nil {
		fail("%v", err)
	}
	// Go's PRNG differs from Python's, so -seed reproduces a deterministic
	// Go-side sequence, not a byte-identical match to the Python benchmark.
	rng := rand.New(rand.NewSource(cfg.seed))

	tiers, err := parseDepthTiers(cfg.ruleDepthTiers)
	if err != nil {
		fail("%v", err)
	}
	numGlobal, numTld, numApex := partitionRuleCounts(cfg.numRules, cfg.globalRuleRate, cfg.tldRuleRate)
	layout := NewRuleLayout(buildKeySizes(numApex, tiers, rand.New(rand.NewSource(layoutSeed))))
	mix := NewRuleMix(numGlobal, numTld, layout)
	printLayoutReport(mix, tiers, cfg.numRules)

	if !cfg.noFlush {
		fmt.Printf("warning: flushing %s before writing\n", describeTarget(cfg))
		if err := conn.FlushDB(ctx).Err(); err != nil {
			fail("flushing database: %v", err)
		}
		fmt.Printf("writing %s rules in batches of %s...\n", comma(int64(cfg.numRules)), comma(int64(cfg.writeBatchSize)))
		numWritten, numBytes, writeElapsed, err := runWritePhase(store, cfg.numRules, cfg.writeBatchSize, mix)
		if err != nil {
			fail("write phase: %v", err)
		}
		printWriteReport(numWritten, numBytes, writeElapsed)
	}

	queries := buildQueries(cfg.numRules, cfg.numQueries, cfg.missRate, rng, mix)

	// The write phase above always goes direct to Dragonfly, regardless of
	// -target: routerd has no Put RPC, by design (a pure client's only
	// business with this system is Lookup). -target only changes which
	// implementation answers the query phase that follows.
	queryTarget, err := parseTarget(cfg.target)
	if err != nil {
		fail("%v", err)
	}
	var queryStore Lookuper = store
	if queryTarget != nil {
		grpcStore, err := NewGRPCLookuper(ctx, queryTarget)
		if err != nil {
			fail("%v", err)
		}
		defer grpcStore.Close()
		queryStore = grpcStore
	}

	if cfg.queryConcurrency > 1 {
		fmt.Printf("\nwarming up %d connection(s)...\n", cfg.queryConcurrency)
		warmUpConnections(queryStore, mode, cfg.queryConcurrency, rng)
	}

	fmt.Printf("\nrunning %s queries (mode=%s, concurrency=%d, target=%s)...\n",
		comma(int64(len(queries))), mode, cfg.queryConcurrency, cfg.target)
	latencies, samples, topSlow, queryElapsed, numErrors := runQueryPhase(queryStore, queries, mode, cfg.queryConcurrency, cfg.slowLatencyGate)
	printLatencyReport(computeLatencyStats(latencies), len(queries), queryElapsed, cfg.queryConcurrency, numErrors)
	printSampleReport(samples)
	printTopSlowReport(topSlow, mode, cfg.numBuckets, cfg.target, cfg.slowLatencyGate)
	if psl, ok := queryStore.(PoolStatsLookuper); ok {
		printPoolStatsSummary(psl.PoolStats())
	}
	if numErrors > 0 {
		os.Exit(1)
	}
}
