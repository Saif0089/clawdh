package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawdh/internal/accounts"
	"clawdh/internal/httpserver"
	"clawdh/internal/shellrc"
)

// Only a terminal counts. The check decides whether the server updates itself,
// and the old one asked "is this a character device?" — which the null device
// is (/dev/null, Windows' NUL), so a server whose output a desktop's autostart
// threw away would never update, and say nothing.
func TestOnlyATerminalCountsAsATerminal(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "clawdh.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if isTerminal(file) {
		t.Error("a log file counts as a terminal")
	}

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Errorf("%s counts as a terminal, so a server writing there would never update itself", os.DevNull)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(w) {
		t.Error("a pipe counts as a terminal")
	}
}

// A machine that is not updating itself says why, on its page and to the panel.
// The reasons used to reach the log alone, so a PC that had quietly stopped
// updating looked like one nobody had superseded.
func TestAReasonUpdatesAreOffReachesThePanel(t *testing.T) {
	t.Cleanup(func() { setUpdateTrouble(nil) })
	srv := httpserver.New(accounts.NewManager(accounts.NewStore(filepath.Join(t.TempDir(), "accounts.json")), t.TempDir()), shellrc.NewSyncer(t.TempDir()), "claude")

	updatesOff(srv, "automatic updates are off: clawdh is running in a terminal")
	if got := updateErrorNow(); !strings.Contains(got, "running in a terminal") {
		t.Errorf("the check-in would report %q", got)
	}
}

// Updates switched off on purpose (CLAWDH_AUTO_UPDATE=0) are reported as such
// rather than as a machine that is fine.
func TestUpdatesSwitchedOffAreReported(t *testing.T) {
	t.Cleanup(func() { setUpdateTrouble(nil) })
	t.Setenv("CLAWDH_AUTO_UPDATE", "0")
	srv := httpserver.New(accounts.NewManager(accounts.NewStore(filepath.Join(t.TempDir(), "accounts.json")), t.TempDir()), shellrc.NewSyncer(t.TempDir()), "claude")

	startAutoUpdate(t.Context(), srv)
	if got := updateErrorNow(); !strings.Contains(got, "CLAWDH_AUTO_UPDATE") {
		t.Errorf("the check-in would report %q, want the switch named", got)
	}
}
