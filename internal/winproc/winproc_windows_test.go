//go:build windows

package winproc

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// Hide adds the no-window flag and leaves what the caller already asked for.
func TestHideAddsNoWindowAndKeepsOtherFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	Hide(cmd)

	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Error("CREATE_NO_WINDOW is not set")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("a flag the caller set was dropped")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow is not set")
	}
}

// A hidden child still runs and still hands its output back through a pipe: the
// helper must not break the notification, the claude probes or schtasks, which
// all read what the program printed.
func TestACommandWithoutAWindowStillRunsAndReports(t *testing.T) {
	out, err := Command("cmd", "/c", "echo", "no-window-ok").Output()
	if err != nil {
		t.Fatalf("running a hidden command: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "no-window-ok" {
		t.Errorf("output = %q, want %q", got, "no-window-ok")
	}
}

// consoleWindowHelper is the child the next test starts: it reports whether it
// has a console window. It runs only when started by that test.
func TestConsoleWindowHelper(t *testing.T) {
	if os.Getenv("CLAWDH_CONSOLE_WINDOW_HELPER") == "" {
		t.Skip("only runs as the child of TestAHiddenChildHasNoConsoleWindow")
	}
	hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	fmt.Printf("consolewindow=%d\n", hwnd)
}

// The point of the package: a program started through it has no console window
// to show, which is what stops a terminal or PowerShell window opening.
func TestAHiddenChildHasNoConsoleWindow(t *testing.T) {
	cmd := Command(os.Args[0], "-test.run=^TestConsoleWindowHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "CLAWDH_CONSOLE_WINDOW_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the helper: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "consolewindow=0") {
		t.Errorf("the child has a console window:\n%s", out)
	}
}
