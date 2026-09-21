package panel

import (
	"testing"
	"time"
)

// ceilingWindow is the window a % ceiling is measured against: an account's
// own, or for a person the fullest among the accounts they can use right now.
func TestCeilingWindow(t *testing.T) {
	now := time.Now()
	windows := []AccountWindow{
		{AccountID: "a1", SevenD: 0.2},
		{AccountID: "a2", SevenD: 0.6},
		{AccountID: "a3", SevenD: 0.9},
	}
	d := Data{
		Accounts: []Account{{ID: "a1", Name: "one"}, {ID: "a2", Name: "two"}, {ID: "a3", Name: "three"}, {ID: "a4", Name: "four"}},
		Shares: []Share{
			{AccountID: "a1", PersonID: "p1"},
			{AccountID: "a2", PersonID: "p1"},
			{AccountID: "a3", PersonID: "p1", ExpiresAt: now.Add(-time.Hour)}, // a loan that has ended
			{AccountID: "a3", PersonID: "p2"},
			{AccountID: "a4", PersonID: "p3"}, // shared, but no reading yet
		},
	}
	for _, tc := range []struct {
		name     string
		limit    Limit
		want     string
		wantOK   bool
		wantFill float64
	}{
		{"an account's own window", Limit{SubjectType: "account", SubjectID: "a2"}, "a2", true, 0.6},
		{"an account with no reading", Limit{SubjectType: "account", SubjectID: "a4"}, "", false, 0},
		{"a person's fullest live share", Limit{SubjectType: "person", SubjectID: "p1"}, "a2", true, 0.6},
		{"a person whose only reading is expired", Limit{SubjectType: "person", SubjectID: "p2"}, "a3", true, 0.9},
		{"a person with shares but no readings", Limit{SubjectType: "person", SubjectID: "p3"}, "", false, 0},
		{"a person with nothing shared", Limit{SubjectType: "person", SubjectID: "p9"}, "", false, 0},
	} {
		w, ok := ceilingWindow(d, windows, tc.limit)
		if ok != tc.wantOK || w.AccountID != tc.want || w.SevenD != tc.wantFill {
			t.Errorf("%s: ceilingWindow = %+v, %v; want account %q fill %v ok %v", tc.name, w, ok, tc.want, tc.wantFill, tc.wantOK)
		}
	}
}
