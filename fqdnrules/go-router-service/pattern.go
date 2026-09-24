// Pure domain-matching logic for the FQDN-pair router: no Redis, no I/O.
//
// A concrete query fqdn is matched against registered patterns by walking its
// own ancestor chain, from the exact literal down to the bare "*" tier, and
// checking at each level whether a rule was registered there. SingleLevel
// (RFC 6125 / TLS-SNI) substitutes exactly one label; MultiLevel matches any
// descendant depth below the wildcard anchor, deepest registered ancestor
// winning. SingleLevel's candidate list is always a subsequence of
// MultiLevel's for the same fqdn, so both modes share one storage layout, one
// write path, and one lookup script.
package router

import (
	"fmt"
	"hash/crc32"
	"strings"
)

const (
	maxLabels  = 20
	globalRank = 255
)

type InvalidFqdnError struct{ msg string }

func (e *InvalidFqdnError) Error() string { return e.msg }

func invalidFqdn(format string, args ...interface{}) *InvalidFqdnError {
	return &InvalidFqdnError{msg: fmt.Sprintf(format, args...)}
}

type MatchMode int

const (
	SingleLevel MatchMode = iota
	MultiLevel
)

func (m MatchMode) String() string {
	if m == SingleLevel {
		return "single"
	}
	return "multi"
}

// Per-rule declaration of how deep a rule's own wildcard may reach. Applies
// only to a rule reached via a wildcard candidate; the exact (rank 0)
// candidate is never scope-filtered.
type WildcardScope byte

const (
	ScopeSingle WildcardScope = 'S'
	ScopeMulti  WildcardScope = 'M'
	ScopeBoth   WildcardScope = 'B'
)

// The zero value is not a valid scope byte, so it maps to the BOTH default
// that Python's WildcardScope.BOTH dataclass default supplies.
func (s WildcardScope) storageByte() byte {
	if s == 0 {
		return byte(ScopeBoth)
	}
	return byte(s)
}

// One pattern string a concrete fqdn could be matched by, with its precedence
// rank and the hash-tag group its Redis key belongs to. Within a list returned
// by candidates(), rank strictly increases and index order is precedence order.
type Candidate struct {
	Pattern    string
	Rank       int
	NeedsMulti bool // only reachable under MultiLevel
	Tag        string
}

func normalizeFqdn(fqdn string) (string, error) {
	if fqdn == "" {
		return "", invalidFqdn("empty fqdn")
	}
	if fqdn == "*" {
		return "", invalidFqdn("'*' is a pattern token, not a concrete fqdn to look up")
	}
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(fqdn), "."))
	if strings.Contains(normalized, "*") {
		return "", invalidFqdn("concrete fqdn cannot contain '*': %q", fqdn)
	}
	labels := strings.Split(normalized, ".")
	for _, label := range labels {
		if label == "" {
			return "", invalidFqdn("empty label in %q", fqdn)
		}
	}
	if len(labels) > maxLabels {
		return "", invalidFqdn("%q has %d labels, exceeds maxLabels=%d", fqdn, len(labels), maxLabels)
	}
	return normalized, nil
}

// Which of the three co-location groups a pattern's Redis key belongs to.
// Same public-suffix caveat as any last-two-labels heuristic: wrong for a
// two-label suffix like .co.uk without a real public-suffix list.
func hashTag(pattern string) string {
	if pattern == "*" {
		return "__global__"
	}
	var remaining []string
	if strings.HasPrefix(pattern, "*.") {
		remaining = strings.Split(pattern[2:], ".")
	} else {
		remaining = strings.Split(pattern, ".")
	}
	if len(remaining) == 1 {
		return "__tld__:" + remaining[0]
	}
	return remaining[len(remaining)-2] + "." + remaining[len(remaining)-1]
}

// True for the global and per-TLD tiers, the tiers checked on every lookup
// regardless of which domain is queried, and so the ones that need bucketed
// replication. The apex tier already spreads across one key per registrable
// domain and must never be bucketed.
func isBucketedTag(tag string) bool {
	return tag == "__global__" || strings.HasPrefix(tag, "__tld__:")
}

func bucketedTag(baseTag string, numBuckets, bucketIndex int) string {
	if numBuckets <= 1 {
		return baseTag
	}
	return fmt.Sprintf("%s:%d", baseTag, bucketIndex)
}

func newCandidate(pattern string, rank int, needsMulti bool, bucketIndex, numBuckets int) Candidate {
	tag := hashTag(pattern)
	if isBucketedTag(tag) {
		tag = bucketedTag(tag, numBuckets, bucketIndex)
	}
	return Candidate{Pattern: pattern, Rank: rank, NeedsMulti: needsMulti, Tag: tag}
}

// Every pattern that could match fqdn under mode, most specific first. fqdn
// must already be normalizeFqdn-clean. With numBuckets > 1 the bucketed tiers
// resolve to one replica chosen by hashing fqdn itself, since their patterns
// ("*", "*.com") are the same fixed string for every query.
func candidates(fqdn string, mode MatchMode, numBuckets int) []Candidate {
	labels := strings.Split(fqdn, ".")
	n := len(labels)
	bucketIndex := 0
	if numBuckets > 1 {
		// ChecksumIEEE returns uint32 and Go's int is 64-bit on both targets, so
		// the converted value is never negative and the modulo is non-negative.
		bucketIndex = int(crc32.ChecksumIEEE([]byte(fqdn))) % numBuckets
	}
	result := []Candidate{newCandidate(fqdn, 0, false, bucketIndex, numBuckets)}
	if n >= 2 {
		parent := strings.Join(labels[1:], ".")
		result = append(result, newCandidate("*."+parent, 1, false, bucketIndex, numBuckets))
	}
	if mode == MultiLevel {
		for k := 2; k < n; k++ {
			remaining := strings.Join(labels[k:], ".")
			result = append(result, newCandidate("*."+remaining, k, true, bucketIndex, numBuckets))
		}
	}
	result = append(result, newCandidate("*", globalRank, false, bucketIndex, numBuckets))
	return result
}

// FieldCandidate is a fqdn2-side lookup candidate: a pattern that could match
// the queried fqdn2, in ascending specificity order. It carries no hash-tag
// or bucket data because fqdn2 is a hash field, never a Redis key -- computing
// a Tag for it would be wasted work.
type FieldCandidate struct {
	Pattern    string
	Rank       int
	NeedsMulti bool
}

// fieldCandidates is candidates() without the hash-tag/bucket-index work,
// for the fqdn2/field side. Same rank/precedence semantics: index 0 is
// always the exact (rank 0) candidate, since this list is never grouped or
// reordered before becoming a lookup's field list.
func fieldCandidates(fqdn string, mode MatchMode) []FieldCandidate {
	labels := strings.Split(fqdn, ".")
	n := len(labels)
	result := []FieldCandidate{{Pattern: fqdn, Rank: 0, NeedsMulti: false}}
	if n >= 2 {
		parent := strings.Join(labels[1:], ".")
		result = append(result, FieldCandidate{Pattern: "*." + parent, Rank: 1, NeedsMulti: false})
	}
	if mode == MultiLevel {
		for k := 2; k < n; k++ {
			remaining := strings.Join(labels[k:], ".")
			result = append(result, FieldCandidate{Pattern: "*." + remaining, Rank: k, NeedsMulti: true})
		}
	}
	result = append(result, FieldCandidate{Pattern: "*", Rank: globalRank, NeedsMulti: false})
	return result
}
