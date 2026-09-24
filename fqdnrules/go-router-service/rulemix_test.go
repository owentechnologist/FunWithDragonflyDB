package router

import "testing"

func TestPartitionRuleCounts(t *testing.T) {
	cases := []struct {
		name                          string
		numRules                      int
		globalRate, tldRate           float64
		wantGlobal, wantTld, wantApex int
	}{
		{"defaults", 100000, 0.05, 0.10, 5000, 10000, 85000},
		{"rates fill the whole space", 1000, 0.25, 0.75, 250, 750, 0},
		{"no catch-alls", 1000, 0.0, 0.0, 0, 0, 1000},
		{"rounding, seven rules", 7, 0.33, 0.33, 2, 2, 3},
		{"rounding, thirteen rules", 13, 0.33, 0.33, 4, 4, 5},
		{"rounding, one rule", 1, 0.5, 0.5, 1, 0, 0},
	}
	for _, c := range cases {
		gotGlobal, gotTld, gotApex := partitionRuleCounts(c.numRules, c.globalRate, c.tldRate)
		if gotGlobal != c.wantGlobal || gotTld != c.wantTld || gotApex != c.wantApex {
			t.Errorf("%s: partitionRuleCounts(%d, %v, %v) = (%d, %d, %d), want (%d, %d, %d)",
				c.name, c.numRules, c.globalRate, c.tldRate,
				gotGlobal, gotTld, gotApex, c.wantGlobal, c.wantTld, c.wantApex)
		}
		if gotGlobal+gotTld+gotApex != c.numRules {
			t.Errorf("%s: partitionRuleCounts(%d, %v, %v) counts sum to %d, want %d",
				c.name, c.numRules, c.globalRate, c.tldRate,
				gotGlobal+gotTld+gotApex, c.numRules)
		}
		if gotApex < 0 {
			t.Errorf("%s: partitionRuleCounts(%d, %v, %v) apex count = %d, want >= 0",
				c.name, c.numRules, c.globalRate, c.tldRate, gotApex)
		}
	}
}

func TestRuleMixTier(t *testing.T) {
	mix := NewRuleMix(3, 5, NewRuleLayout([]int{2, 2}))
	cases := []struct {
		i         int
		wantTier  RuleTier
		wantIndex int
	}{
		{0, TierGlobal, 0},
		{2, TierGlobal, 2},
		{3, TierTld, 0},
		{7, TierTld, 4},
		{8, TierApex, 0},
		{11, TierApex, 3},
	}
	for _, c := range cases {
		gotTier, gotIndex := mix.Tier(c.i)
		if gotTier != c.wantTier || gotIndex != c.wantIndex {
			t.Errorf("Tier(%d) = (%d, %d), want (%d, %d)", c.i, gotTier, gotIndex, c.wantTier, c.wantIndex)
		}
	}
}

func TestRuleMixCounts(t *testing.T) {
	mix := NewRuleMix(3, 5, NewRuleLayout([]int{2, 2}))
	if got := mix.NumGlobal(); got != 3 {
		t.Errorf("NumGlobal() = %d, want 3", got)
	}
	if got := mix.NumTld(); got != 5 {
		t.Errorf("NumTld() = %d, want 5", got)
	}
	if got := mix.NumApex(); got != 4 {
		t.Errorf("NumApex() = %d, want 4", got)
	}
}

func TestValidateRuleMixRates(t *testing.T) {
	cases := []struct {
		globalRate, tldRate float64
		wantErr             bool
	}{
		{0.05, 0.10, false},
		{0.0, 0.0, false},
		{0.5, 0.5, false},
		{-0.1, 0.10, true},
		{0.05, -0.1, true},
		{1.5, 0.0, true},
		{0.0, 1.5, true},
		{0.6, 0.6, true},
	}
	for _, c := range cases {
		err := validateRuleMixRates(c.globalRate, c.tldRate)
		if (err != nil) != c.wantErr {
			t.Errorf("validateRuleMixRates(%v, %v) = %v, want error: %v", c.globalRate, c.tldRate, err, c.wantErr)
		}
	}
}
