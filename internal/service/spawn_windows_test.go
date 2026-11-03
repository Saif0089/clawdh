//go:build windows

package service

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"clawdh/internal/config"
	"golang.org/x/sys/windows"
)

// consoleWindowReporter is the program spawnDetached starts in the next test:
// the test binary itself, which prints whether it has a console window and
// exits. It runs only when started that way.
func TestConsoleWindowReporter(t *testing.T) {
	if os.Getenv("CLAWDH_CONSOLE_REPORTER") == "" {
		t.Skip("only runs as the child of TestASpawnedServerHasNoConsoleWindow")
	}
	hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	fmt.Printf("consolewindow=%d\n", hwnd)
}

// The server is started by spawnDetached at login, after an update and from
// `clawdh start`. It must come up without a console window: a visible one stood
// on screen for the server's whole life, and closing it ended clawdh.
func TestASpawnedServerHasNoConsoleWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("CLAWDH_CONSOLE_REPORTER", "1")

	pid, err := spawnDetached(os.Args[0], []string{"-test.run=^TestConsoleWindowReporter$", "-test.v"})
	if err != nil {
		t.Fatalf("spawnDetached: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	logPath, err := config.LogFile()
	if err != nil {
		t.Fatal(err)
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "consolewindow=0") {
		t.Errorf("the spawned process has a console window (or never reported):\n%s", logged)
	}
}
