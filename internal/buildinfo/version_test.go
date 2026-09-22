package buildinfo

import "testing"

// A stamped subject is what turns "you were updated" into "you were updated,
// and here is what changed" — so Describe has to add it when it is there and
// change nothing at all when it isn't.
func TestDescribe(t *testing.T) {
	defer restore(Version, Commit, Summary)()

	Version, Commit, Summary = "latest", "abc1234def", "feat(usage): the board is per account"
	if got, want := Describe(), "latest · abc1234 — feat(usage): the board is per account"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}

	// No stamp: an ad-hoc build reads exactly as it did before this existed.
	Summary = ""
	if got, want := Describe(), Tag(); got != want {
		t.Errorf("unstamped Describe() = %q, want the bare tag %q", got, want)
	}

	// Whitespace is not a summary.
	Summary = "   "
	if got, want := Describe(), Tag(); got != want {
		t.Errorf("blank Describe() = %q, want the bare tag %q", got, want)
	}
}

// A long subject gets cut, and the cut says so — nobody should read a
// truncated sentence as the whole of it.
func TestHeadlineTruncates(t *testing.T) {
	defer restore(Version, Commit, Summary)()

	short := "fix: a small thing"
	Summary = short
	if got := Headline(); got != short {
		t.Errorf("a short subject was altered: %q", got)
	}

	Summary = "feat(gateway): nginx caps the body at its 1m default, which Claude Code shows as the 32MB limit it is not"
	got := Headline()
	if []rune(got)[len([]rune(got))-1] != '…' {
		t.Errorf("a cut subject should end in an ellipsis, got %q", got)
	}
	if n := len([]rune(got)); n > summaryLimit+1 {
		t.Errorf("Headline() is %d runes, want at most %d plus the ellipsis", n, summaryLimit)
	}

	// Cutting by rune, not by byte: a subject that is all multi-byte characters
	// must come back as whole characters. Cutting bytes here would produce
	// mojibake in a status badge.
	Summary = ""
	for i := 0; i < summaryLimit+20; i++ {
		Summary += "é"
	}
	for _, r := range Headline() {
		if r != 'é' && r != '…' {
			t.Fatalf("rune-unsafe cut produced %q in %q", r, Headline())
		}
	}
}

func restore(version, commit, summary string) func() {
	return func() { Version, Commit, Summary = version, commit, summary }
}
