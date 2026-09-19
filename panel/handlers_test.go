package panel

import "testing"

// A login named by its email is typed by the part before the @; two shares
// that would collide get a stable -2 suffix, in account-id order.
func TestShareSlugsShortenEmailsAndStayUnique(t *testing.T) {
	if got := shareSlug("ehtisham@devhouse.co"); got != "ehtisham" {
		t.Fatalf("shareSlug(email) = %q, want ehtisham", got)
	}
	if got := shareSlug("Work Laptop"); got != "work-laptop" {
		t.Fatalf("shareSlug(name) = %q, want work-laptop", got)
	}
	d := Data{
		Accounts: []Account{
			{ID: "b", Name: "ehtisham@other.io"},
			{ID: "a", Name: "ehtisham@devhouse.co"},
			{ID: "c", Name: "HassanDH"},
		},
		Shares: []Share{{PersonID: "p", AccountID: "a"}, {PersonID: "p", AccountID: "b"}, {PersonID: "p", AccountID: "c"}, {PersonID: "q", AccountID: "b"}},
	}
	got := shareSlugs(d, "p")
	want := map[string]string{"a": "ehtisham", "b": "ehtisham-2", "c": "hassandh"}
	for id, slug := range want {
		if got[id] != slug {
			t.Errorf("slug for %s = %q, want %q", id, got[id], slug)
		}
	}
	if q := shareSlugs(d, "q"); q["b"] != "ehtisham" {
		t.Errorf("the other person's only share should be plain: %q", q["b"])
	}
}
