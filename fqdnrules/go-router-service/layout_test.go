package router

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

const eps = 1e-9

func TestParseDepthTiersDefault(t *testing.T) {
	tiers, err := parseDepthTiers(defaultDepthTiers)
	if err != nil {
		t.Fatalf("parseDepthTiers(%q) returned error: %v", defaultDepthTiers, err)
	}
	want := []DepthTier{
		{Weight: 0.5, MinSize: 1, MaxSize: 3},
		{Weight: 0.3, MinSize: 4, MaxSize: 20},
		{Weight: 0.1, MinSize: 21, MaxSize: 50},
		{Weight: 0.1, MinSize: 51, MaxSize: 2000},
	}
	if len(tiers) != 4 {
		t.Fatalf("got %d tiers, want 4", len(tiers))
	}
	for i, w := range want {
		got := tiers[i]
		if math.Abs(got.Weight-w.Weight) > eps {
			t.Errorf("tier %d weight = %v, want %v", i, got.Weight, w.Weight)
		}
		if got.MinSize != w.MinSize || got.MaxSize != w.MaxSize {
			t.Errorf("tier %d range = (%d, %d), want (%d, %d)", i, got.MinSize, got.MaxSize, w.MinSize, w.MaxSize)
		}
	}
}

func TestParseDepthTiersRenormalizes(t *testing.T) {
	tiers, err := parseDepthTiers("1:1-2,3:3-4")
	if err != nil {
		t.Fatalf("parseDepthTiers returned error: %v", err)
	}
	if len(tiers) != 2 {
		t.Fatalf("got %d tiers, want 2", len(tiers))
	}
	if math.Abs(tiers[0].Weight-0.25) > eps {
		t.Errorf("tier 0 weight = %v, want 0.25", tiers[0].Weight)
	}
	if math.Abs(tiers[1].Weight-0.75) > eps {
		t.Errorf("tier 1 weight = %v, want 0.75", tiers[1].Weight)
	}
	if tiers[0].MinSize != 1 || tiers[0].MaxSize != 2 {
		t.Errorf("tier 0 range = (%d, %d), want (1, 2)", tiers[0].MinSize, tiers[0].MaxSize)
	}
	if tiers[1].MinSize != 3 || tiers[1].MaxSize != 4 {
		t.Errorf("tier 1 range = (%d, %d), want (3, 4)", tiers[1].MinSize, tiers[1].MaxSize)
	}
}

func TestParseDepthTiersRejects(t *testing.T) {
	cases := []struct {
		spec    string
		wantMsg string
	}{
		{"50", `invalid depth tier "50", expected PCT:LO-HI`},
		{"50:13", `invalid depth tier "50:13", expected PCT:LO-HI`},
		{"abc:1-3", `invalid depth tier "abc:1-3", expected PCT:LO-HI`},
		{"50:x-3", `invalid depth tier "50:x-3", expected PCT:LO-HI`},
		{"50:1-y", `invalid depth tier "50:1-y", expected PCT:LO-HI`},
		{"0:1-3", `depth tier percentage must be > 0, got "0:1-3"`},
		{"-5:1-3", `depth tier percentage must be > 0, got "-5:1-3"`},
		{"50:0-3", `depth tier range must satisfy 1 <= lo <= hi, got "50:0-3"`},
		{"50:5-2", `depth tier range must satisfy 1 <= lo <= hi, got "50:5-2"`},
	}
	for _, c := range cases {
		tiers, err := parseDepthTiers(c.spec)
		if err == nil {
			t.Errorf("parseDepthTiers(%q) = %v, want error", c.spec, tiers)
			continue
		}
		if !strings.Contains(err.Error(), c.wantMsg) {
			t.Errorf("parseDepthTiers(%q) error = %q, want it to contain %q", c.spec, err.Error(), c.wantMsg)
		}
	}
}

func TestBuildKeySizes(t *testing.T) {
	tiers, err := parseDepthTiers(defaultDepthTiers)
	if err != nil {
		t.Fatalf("parseDepthTiers returned error: %v", err)
	}
	for _, numRules := range []int{1000, 12345, 2} {
		rng := rand.New(rand.NewSource(layoutSeed))
		sizes := buildKeySizes(numRules, tiers, rng)
		if len(sizes) == 0 {
			t.Fatalf("buildKeySizes(%d) returned no sizes", numRules)
		}
		total := 0
		for _, size := range sizes {
			if size < 1 {
				t.Errorf("buildKeySizes(%d) produced size %d, want >= 1", numRules, size)
			}
			if !inSomeTier(size, tiers) {
				t.Errorf("buildKeySizes(%d) produced size %d outside every tier band", numRules, size)
			}
			total += size
		}
		if total != numRules {
			t.Errorf("buildKeySizes(%d) sizes sum to %d, want %d", numRules, total, numRules)
		}
	}
}

func TestBuildKeySizesSingleRule(t *testing.T) {
	tiers, err := parseDepthTiers(defaultDepthTiers)
	if err != nil {
		t.Fatalf("parseDepthTiers returned error: %v", err)
	}
	sizes := buildKeySizes(1, tiers, rand.New(rand.NewSource(layoutSeed)))
	if len(sizes) != 1 || sizes[0] != 1 {
		t.Errorf("buildKeySizes(1) = %v, want [1]", sizes)
	}
}

func inSomeTier(size int, tiers []DepthTier) bool {
	for _, t := range tiers {
		if size >= t.MinSize && size <= t.MaxSize {
			return true
		}
	}
	return false
}

func TestKeyForRule(t *testing.T) {
	cases := []struct {
		keySizes []int
		numKeys  int
		want     [][2]int
	}{
		{
			keySizes: []int{3, 1, 2},
			numKeys:  3,
			want:     [][2]int{{0, 0}, {0, 1}, {0, 2}, {1, 0}, {2, 0}, {2, 1}},
		},
		{
			keySizes: []int{1, 1, 1},
			numKeys:  3,
			want:     [][2]int{{0, 0}, {1, 0}, {2, 0}},
		},
	}
	for _, c := range cases {
		layout := NewRuleLayout(c.keySizes)
		if got := layout.NumKeys(); got != c.numKeys {
			t.Errorf("NewRuleLayout(%v).NumKeys() = %d, want %d", c.keySizes, got, c.numKeys)
		}
		for i, w := range c.want {
			keyID, pos := layout.KeyForRule(i)
			if keyID != w[0] || pos != w[1] {
				t.Errorf("NewRuleLayout(%v).KeyForRule(%d) = (%d, %d), want (%d, %d)", c.keySizes, i, keyID, pos, w[0], w[1])
			}
		}
	}
}
