package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
)

const defaultDepthTiers = "50:1-3,30:4-20,10:21-50,10:51-2000"

// Fixed, deliberately independent of the benchmark's --seed, so a --no-flush rerun
// with a different --seed resamples queries against the same previously-written keys.
const layoutSeed = 1337

// DepthTier is one weighted band of the rule-depth distribution: this fraction of
// keys get a rule count sampled uniformly from [MinSize, MaxSize].
type DepthTier struct {
	Weight  float64
	MinSize int
	MaxSize int
}

// parseDepthTiers parses "PCT:LO-HI,PCT:LO-HI,...". Percentages need not sum to
// 100, they are renormalized into weights, but must be positive.
func parseDepthTiers(spec string) ([]DepthTier, error) {
	parts := strings.Split(spec, ",")
	tiers := make([]DepthTier, 0, len(parts))
	for _, part := range parts {
		pctStr, rangeStr, hasColon := strings.Cut(part, ":")
		loStr, hiStr, hasDash := strings.Cut(rangeStr, "-")
		if !hasColon || !hasDash {
			return nil, fmt.Errorf("invalid depth tier %q, expected PCT:LO-HI", part)
		}
		pct, err := strconv.ParseFloat(pctStr, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid depth tier %q, expected PCT:LO-HI: %w", part, err)
		}
		lo, err := strconv.Atoi(loStr)
		if err != nil {
			return nil, fmt.Errorf("invalid depth tier %q, expected PCT:LO-HI: %w", part, err)
		}
		hi, err := strconv.Atoi(hiStr)
		if err != nil {
			return nil, fmt.Errorf("invalid depth tier %q, expected PCT:LO-HI: %w", part, err)
		}
		if pct <= 0 {
			return nil, fmt.Errorf("depth tier percentage must be > 0, got %q", part)
		}
		if lo < 1 || hi < lo {
			return nil, fmt.Errorf("depth tier range must satisfy 1 <= lo <= hi, got %q", part)
		}
		tiers = append(tiers, DepthTier{Weight: pct, MinSize: lo, MaxSize: hi})
	}
	totalPct := 0.0
	for _, t := range tiers {
		totalPct += t.Weight
	}
	for i := range tiers {
		tiers[i].Weight /= totalPct
	}
	return tiers, nil
}

// buildKeySizes partitions numRules rules into keys, one size per key, each size
// drawn from a weighted pick among tiers then a uniform draw within that tier's
// [MinSize, MaxSize]. The last key is trimmed so sizes sum to exactly numRules.
func buildKeySizes(numRules int, tiers []DepthTier, rng *rand.Rand) []int {
	totalWeight := 0.0
	for _, t := range tiers {
		totalWeight += t.Weight
	}
	var sizes []int
	total := 0
	for total < numRules {
		tier := pickTier(tiers, rng.Float64()*totalWeight)
		size := tier.MinSize + rng.Intn(tier.MaxSize-tier.MinSize+1)
		if remaining := numRules - total; size > remaining {
			size = remaining
		}
		sizes = append(sizes, size)
		total += size
	}
	return sizes
}

// draw is scaled to the tiers' own weight sum, matching Python's
// random.choices, so unnormalized weights behave the same as normalized ones.
func pickTier(tiers []DepthTier, draw float64) DepthTier {
	cum := 0.0
	for _, t := range tiers {
		cum += t.Weight
		if draw < cum {
			return t
		}
	}
	// Rounding can leave the final cumulative bound just under the draw.
	return tiers[len(tiers)-1]
}

// RuleLayout maps a global rule index to the key it is nested under and its
// position within that key.
type RuleLayout struct {
	KeySizes []int
	prefix   []int
}

func NewRuleLayout(keySizes []int) *RuleLayout {
	prefix := make([]int, len(keySizes))
	sum := 0
	for i, size := range keySizes {
		sum += size
		prefix[i] = sum
	}
	return &RuleLayout{KeySizes: keySizes, prefix: prefix}
}

func (l *RuleLayout) NumKeys() int {
	return len(l.KeySizes)
}

// KeyForRule returns the key ID owning global rule index i and i's position
// within that key.
func (l *RuleLayout) KeyForRule(i int) (int, int) {
	keyID := sort.Search(len(l.prefix), func(j int) bool { return l.prefix[j] > i })
	if keyID == 0 {
		return 0, i
	}
	return keyID, i - l.prefix[keyID-1]
}
