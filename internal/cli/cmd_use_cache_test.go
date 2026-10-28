package cli

import (
	"os"
	"strings"
	"testing"
)

// unsetForTest removes a variable for the rest of the test and restores it
// afterwards.
func unsetForTest(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "") // registers the restore
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func envCount(env []string, prefix string) (n int) {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// A shared session keeps its prompt cache for an hour, as the same login would
// signed in directly — unless the person has decided the cache themselves.
func TestASharedSessionKeepsItsCacheForAnHour(t *testing.T) {
	for _, name := range cacheChoices {
		unsetForTest(t, name)
	}
	if got := sharedTarget("https://gw.example", "k", "team").env; envCount(got, "ENABLE_PROMPT_CACHING_1H=1") != 1 {
		t.Errorf("a shared session's env has no hour-long cache: %v", got)
	}

	for _, choice := range []string{"FORCE_PROMPT_CACHING_5M=1", "CLAUDE_CODE_PROMPT_CACHE_TTL=5m", "ENABLE_PROMPT_CACHING_1H=0"} {
		name, value, _ := strings.Cut(choice, "=")
		t.Setenv(name, value)
		got := sharedTarget("https://gw.example", "k", "team").env
		if envCount(got, choice) != 1 || envCount(got, "ENABLE_PROMPT_CACHING_1H=1") != 0 {
			t.Errorf("with %s set, the env is %v; want the person's choice kept and nothing added", choice, got)
		}
		unsetForTest(t, name)
	}
}
