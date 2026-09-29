package switching

import (
	"path/filepath"
	"testing"
)

func TestParseRuleArgs(t *testing.T) {
	for _, c := range []struct {
		args   []string
		window string
		pct    float64
		ok     bool
	}{
		{[]string{"at", "5%"}, WindowWeek, 5, true},
		{[]string{"at", "5"}, WindowWeek, 5, true},
		{[]string{"at", "80%", "5h"}, Window5h, 80, true},
		{[]string{"AT", "12.5%", "week"}, WindowWeek, 12.5, true},
		{[]string{"at", "0%"}, WindowWeek, 0, true},
		// Not rules: they are left to be whatever they were before.
		{[]string{"at", "150%"}, "", 0, false},
		{[]string{"at", "-1"}, "", 0, false},
		{[]string{"at", "five"}, "", 0, false},
		{[]string{"at", "5%", "month"}, "", 0, false},
		{[]string{"-p", "hello"}, "", 0, false},
		{[]string{"at"}, "", 0, false},
		{nil, "", 0, false},
	} {
		window, pct, ok := ParseRuleArgs(c.args)
		if ok != c.ok || window != c.window || pct != c.pct {
			t.Errorf("ParseRuleArgs(%q) = %q, %v, %v; want %q, %v, %v", c.args, window, pct, ok, c.window, c.pct, c.ok)
		}
	}
}

func TestParseRuleTrigger(t *testing.T) {
	for _, c := range []struct {
		prompt string
		want   Rule
		clear  bool
		ok     bool
	}{
		{"clawdh saif at 5%", Rule{Account: "saif", Window: WindowWeek, AtPercent: 5}, false, true},
		{"  clawdh shared saif at 5% 5h ", Rule{Account: "saif", Shared: true, Window: Window5h, AtPercent: 5}, false, true},
		{"clawdh switch ehti at 90", Rule{Account: "ehti", Window: WindowWeek, AtPercent: 90}, false, true},
		{"clawdh stay", Rule{}, true, true},
		// Ordinary prompts, and the immediate switch, are not rules.
		{"clawdh saif", Rule{}, false, false},
		{"clawdh shared saif", Rule{}, false, false},
		{"switch to saif at 5%", Rule{}, false, false},
		{"clawdh saif at 5% please", Rule{}, false, false},
		{"/clawdh saif at 5%", Rule{}, false, false},
	} {
		r, clear, ok := ParseRuleTrigger(c.prompt)
		if ok != c.ok || clear != c.clear || r != c.want {
			t.Errorf("ParseRuleTrigger(%q) = %+v, clear=%v, ok=%v; want %+v, %v, %v", c.prompt, r, clear, ok, c.want, c.clear, c.ok)
		}
	}
	// The rule forms must not be mistaken for an immediate switch either.
	if _, _, ok := ParseTrigger("clawdh saif at 5%"); ok {
		t.Error("ParseTrigger took a rule for an immediate switch")
	}
}

func TestRuleRoundTripAndWords(t *testing.T) {
	handoff := filepath.Join(t.TempDir(), ".handoff-1.json")
	if _, ok := ReadRule(handoff); ok {
		t.Fatal("a rule was read before any was written")
	}
	r := Rule{Account: "saif", Shared: true, Window: WindowWeek, AtPercent: 5, For: "n1"}
	if err := WriteRule(handoff, r); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadRule(handoff)
	if !ok || got.Account != "saif" || !got.Shared || got.AtPercent != 5 || got.For != "n1" {
		t.Fatalf("ReadRule = %+v, %v", got, ok)
	}
	if d := got.Describe(); d != "move to saif at 5% of the week" {
		t.Errorf("Describe = %q", d)
	}
	if b := got.Badge(); b != "→ saif 5%wk" {
		t.Errorf("Badge = %q", b)
	}
	ClearRule(handoff)
	if _, ok := ReadRule(handoff); ok {
		t.Error("the rule survived ClearRule")
	}
}
