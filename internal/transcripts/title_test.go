package transcripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, claudeDir, id string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", "-Users-someone-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	caveat   = `{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>not a prompt</local-command-caveat>"}}`
	toolLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}`
	prompt   = `{"type":"user","message":{"role":"user","content":"fix the   flaky\nlogin test"}}`
	ai1      = `{"type":"ai-title","aiTitle":"Flaky login test","sessionId":"s"}`
	ai2      = `{"type":"ai-title","aiTitle":"Login test retries","sessionId":"s"}`
	custom   = `{"type":"custom-title","customTitle":"my login work","sessionId":"s"}`
)

// A name the person gave beats the generated one, the latest generated one beats
// an earlier one, and with neither the first real prompt names it — never the
// notes Claude Code adds around commands, or a tool result.
func TestTitleIsTheGivenNameThenTheGeneratedOneThenTheFirstPrompt(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a", caveat, toolLine, prompt, ai1, `{"type":"assistant"}`, ai2)
	write(t, root, "b", caveat, prompt, ai1, custom, ai2)
	write(t, root, "c", caveat, toolLine, prompt, `{"type":"assistant"}`)
	write(t, root, "d", caveat)
	for id, want := range map[string]string{
		"a": "Login test retries", "b": "my login work", "c": "fix the flaky login test", "d": "", "missing": "", "../a": "",
	} {
		if got := Title(root, id); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
}

// A transcript bigger than what is read from each end is named from its ends,
// and a changed file is read again rather than served from the cache.
func TestALongTranscriptIsNamedFromItsEndsAndRereadWhenItChanges(t *testing.T) {
	root := t.TempDir()
	filler := `{"type":"assistant","message":{"content":"` + strings.Repeat("x", 1000) + `"}}`
	lines := []string{prompt, ai1}
	for i := 0; i < 2*ends/len(filler)+10; i++ {
		lines = append(lines, filler)
	}
	p := write(t, root, "long", lines...)
	if got := Title(root, "long"); got != "Flaky login test" {
		t.Fatalf("from the head: %q", got)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(custom + "\n")
	f.Close()
	if got := Title(root, "long"); got != "my login work" {
		t.Errorf("after a rename at the end: %q", got)
	}
}

func TestATitleIsKeptToOneReadableLine(t *testing.T) {
	long := strings.Repeat("word ", 40)
	if got := clean(long); len([]rune(got)) != maxTitle || !strings.HasSuffix(got, "…") {
		t.Errorf("clean = %q (%d runes)", got, len([]rune(got)))
	}
}
