package router

import (
	"fmt"
	"math"
)

// Which hash-tag tier (see hashTag in pattern.go) a rule's fqdn1 pattern lands in.
type RuleTier int

const (
	TierGlobal RuleTier = iota
	TierTld
	TierApex
)

// Rounding each rate independently can push the two counts past numRules even
// when the rates themselves sum to 1.0 or less, so the sum is clamped.
func partitionRuleCounts(numRules int, globalRate, tldRate float64) (numGlobal, numTld, numApex int) {
	numGlobal = int(math.Round(float64(numRules) * globalRate))
	numTld = int(math.Round(float64(numRules) * tldRate))
	if numGlobal+numTld > numRules {
		numTld = numRules - numGlobal
		if numTld < 0 {
			numTld = 0
		}
		if numGlobal > numRules {
			numGlobal = numRules
		}
	}
	return numGlobal, numTld, numRules - numGlobal - numTld
}

func validateRuleMixRates(globalRate, tldRate float64) error {
	if globalRate < 0.0 || globalRate > 1.0 {
		return fmt.Errorf("-global-rule-rate must be within [0.0, 1.0], got %v", globalRate)
	}
	if tldRate < 0.0 || tldRate > 1.0 {
		return fmt.Errorf("-tld-rule-rate must be within [0.0, 1.0], got %v", tldRate)
	}
	if globalRate+tldRate > 1.0 {
		return fmt.Errorf("-global-rule-rate plus -tld-rule-rate must be <= 1.0, got %v and %v", globalRate, tldRate)
	}
	return nil
}

// Deliberately far smaller than the apex tier's numDomains. Real TLD-tier
// traffic is coarse, a handful of catch-all patterns each holding many rules as
// distinct fqdn2 hash fields, so a small pool concentrates load the way
// -num-buckets replication is meant to mitigate instead of spreading it thin
// like the apex tier does.
const numSyntheticTlds = 20

// The "ztld" prefix never collides with the real "com" suffix every apex-tier
// domain carries (domainForKey produces dragonflyN.com), so a TLD-tier probe
// can neither shadow nor be shadowed by an apex-tier rule.
func syntheticTld(tierIndex int) string {
	return fmt.Sprintf("ztld%d", tierIndex%numSyntheticTlds)
}

// RuleMix splits the global rule index space into the three fqdn1 tiers. Only
// the apex share is laid out over keys, so layout covers numApex rules.
type RuleMix struct {
	numGlobal int
	numTld    int
	numApex   int
	layout    *RuleLayout
}

func NewRuleMix(numGlobal, numTld int, layout *RuleLayout) *RuleMix {
	numApex := 0
	for _, size := range layout.KeySizes {
		numApex += size
	}
	return &RuleMix{numGlobal: numGlobal, numTld: numTld, numApex: numApex, layout: layout}
}

func (m *RuleMix) Tier(i int) (RuleTier, int) {
	if i < m.numGlobal {
		return TierGlobal, i
	}
	if i < m.numGlobal+m.numTld {
		return TierTld, i - m.numGlobal
	}
	return TierApex, i - m.numGlobal - m.numTld
}

func (m *RuleMix) NumGlobal() int { return m.numGlobal }

func (m *RuleMix) NumTld() int { return m.numTld }

func (m *RuleMix) NumApex() int { return m.numApex }
