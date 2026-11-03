//go:build windows

package updater

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
)

// stubElevation replaces the elevated path for the duration of a test and
// reports whether it was reached. No test may ever raise a real UAC
// prompt: CI runs unattended and a prompt would hang until the job's
// timeout, which is exactly the failure this whole design avoids.
func stubElevation(t *testing.T, err error) *bool {
	t.Helper()
	called := false
	previous := elevatedReplace
	elevatedReplace = func(from, to string) error {
		called = true
		return err
	}
	t.Cleanup(func() { elevatedReplace = previous })
	return &called
}

// TestReplaceBinaryNeverElevatesOnAnOrdinaryInstall is the property that
// keeps every normal machine — and CI — prompt-free. clawdh installs under
// %LOCALAPPDATA%, which the user owns, so the unprivileged rename works
// and the elevated path must not be reached at all.
func TestReplaceBinaryNeverElevatesOnAnOrdinaryInstall(t *testing.T) {
	called := stubElevation(t, errors.New("must not be called"))

	dir := t.TempDir()
	to := filepath.Join(dir, "clawdh.exe")
	from := filepath.Join(dir, "clawdh.exe.new")
	if err := os.WriteFile(to, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(from, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(context.Background(), from, to); err != nil {
		t.Fatalf("replaceBinary on a writable directory: %v", err)
	}
	if *called {
		t.Error("elevation was requested for a directory the user can write; " +
			"an ordinary install must never raise a UAC prompt")
	}
	if got, err := os.ReadFile(to); err != nil || string(got) != "new" {
		t.Errorf("binary content = %q (err %v), want %q", got, err, "new")
	}
}

// TestReplaceBinaryElevatesOnlyOnAccessDenied pins the trigger. Elevation
// must answer "Windows refused for want of rights" and nothing else: a
// missing file or a locked one is not fixed by administrator rights, and
// prompting for them would be noise the user has to dismiss.
func TestReplaceBinaryElevatesOnlyOnAccessDenied(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "clawdh.exe.new")
	if err := os.WriteFile(from, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The target does not exist, so the first rename fails — but with
	// "not found", which elevation cannot help with.
	called := stubElevation(t, nil)
	if err := replaceBinary(context.Background(), from, filepath.Join(dir, "absent.exe")); err == nil {
		t.Error("replacing a binary that is not there should fail")
	}
	if *called {
		t.Error("elevation was requested for a missing file, which administrator rights cannot fix")
	}
}

func TestIsAccessDenied(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"ERROR_ACCESS_DENIED", &os.LinkError{Err: syscall.Errno(5)}, true},
		{"fs.ErrPermission", fs.ErrPermission, true},
		{"wrapped permission error", &os.PathError{Err: fs.ErrPermission}, true},
		{"not found", &os.LinkError{Err: syscall.Errno(2)}, false},
		{"sharing violation", &os.LinkError{Err: syscall.Errno(32)}, false},
		{"nil", nil, false},
		{"unrelated", errors.New("disk full"), false},
	} {
		if got := isAccessDenied(tc.err); got != tc.want {
			t.Errorf("%s: isAccessDenied(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestEncodePowerShellCommand checks the wire format -EncodedCommand
// documents: UTF-16LE then base64. Getting this wrong would send the
// elevated shell garbage, and it would fail only on a machine that had
// already prompted the user.
func TestEncodePowerShellCommand(t *testing.T) {
	const script = "Write-Host 'ok'"
	decoded, err := base64.StdEncoding.DecodeString(encodePowerShellCommand(script))
	if err != nil {
		t.Fatalf("output is not base64: %v", err)
	}
	if len(decoded)%2 != 0 {
		t.Fatalf("decoded length %d is not a whole number of UTF-16 units", len(decoded))
	}
	units := make([]uint16, 0, len(decoded)/2)
	for i := 0; i < len(decoded); i += 2 {
		units = append(units, uint16(decoded[i])|uint16(decoded[i+1])<<8)
	}
	if got := string(utf16.Decode(units)); got != script {
		t.Errorf("round trip = %q, want %q", got, script)
	}
}

// TestPsQuoteEscapesPathsWithQuotes covers the real-world path shapes
// that reach the elevated script — spaces are routine and an apostrophe
// in a Windows user name is legal.
func TestPsQuoteEscapesPathsWithQuotes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`C:\Users\John Smith\clawdh.exe`, `'C:\Users\John Smith\clawdh.exe'`},
		{`C:\Users\O'Brien\clawdh.exe`, `'C:\Users\O''Brien\clawdh.exe'`},
	} {
		if got := psQuote(tc.in); got != tc.want {
			t.Errorf("psQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// copyFileTo copies src to dst, standing a real program in for an executable.
func copyFileTo(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSleepHelper is the long-lived process the next test keeps running: it
// stands in for a `clawdh run` session started before the previous update.
func TestSleepHelper(t *testing.T) {
	if os.Getenv("CLAWDH_SLEEP_HELPER") == "" {
		t.Skip("only runs as the child of TestReplaceBinaryWorksWhileThePreviousImageIsStillRunning")
	}
	time.Sleep(90 * time.Second)
}

// TestReplaceBinaryWorksWhileThePreviousImageIsStillRunning is the update that
// asked for administrator rights on an ordinary per-user install.
//
// An update moves the running clawdh.exe aside as clawdh.exe.old and deletes it
// at the next start. But a session started before that update — `clawdh run`
// keeps one open for hours — is still running that very file, so it can be
// neither deleted nor renamed over, and the NEXT update's swap failed with
// "Access is denied". That was read as "this folder needs administrator
// rights", so a UAC prompt and a PowerShell window appeared on a folder the
// user owns; and since the update loop retries every two minutes, they came
// back every two minutes, and the elevated retry hit the same held file.
func TestReplaceBinaryWorksWhileThePreviousImageIsStillRunning(t *testing.T) {
	called := stubElevation(t, errors.New("must not be called"))

	dir := t.TempDir()
	to := filepath.Join(dir, "clawdh.exe")
	from := filepath.Join(dir, "clawdh.exe.new")
	old := to + OldBinarySuffix
	if err := os.WriteFile(to, []byte("live"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(from, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The displaced image of an earlier update, still running.
	copyFileTo(t, os.Args[0], old)
	child := exec.Command(old, "-test.run=^TestSleepHelper$")
	child.Env = append(os.Environ(), "CLAWDH_SLEEP_HELPER=1")
	if err := child.Start(); err != nil {
		t.Fatalf("starting the stand-in session: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	if err := os.Remove(old); err == nil {
		t.Fatal("precondition: a running image was deleted, so this test cannot reproduce the held file")
	}

	if err := replaceBinary(context.Background(), from, to); err != nil {
		t.Fatalf("an update over a still-running previous image failed: %v", err)
	}
	if *called {
		t.Error("administrator rights were requested for a folder the user can write; " +
			"a held file is not a rights problem")
	}
	if got, err := os.ReadFile(to); err != nil || string(got) != "new" {
		t.Errorf("binary content = %q (err %v), want %q", got, err, "new")
	}
}

// refuseSwap makes Windows refuse the swap with ERROR_ACCESS_DENIED and says
// whether the folder is one this account can write.
func refuseSwap(t *testing.T, writable bool) {
	t.Helper()
	prevSwap, prevWritable := swapInPlace, dirWritable
	swapInPlace = func(from, to string) error {
		return &os.LinkError{Op: "rename", Old: to, New: to + ".old", Err: syscall.Errno(5)}
	}
	dirWritable = func(string) bool { return writable }
	t.Cleanup(func() { swapInPlace, dirWritable = prevSwap, prevWritable })
}

// A refusal in a folder the account CAN write is something holding a file, which
// administrator rights do not fix: no prompt, even for an update a person asked
// for.
func TestARefusalInAWritableFolderNeverAsksForAdministrator(t *testing.T) {
	called := stubElevation(t, nil)
	refuseSwap(t, true)

	err := replaceBinary(WithInteractive(context.Background()), `C:\x\clawdh.exe.new`, `C:\x\clawdh.exe`)
	if err == nil {
		t.Fatal("a refused swap reported success")
	}
	if *called {
		t.Error("administrator rights were requested for a folder this account can write")
	}
}

// The timer's update has nobody to answer a prompt, and it runs every few
// minutes: it says what is needed and leaves the choice to the person.
func TestTheTimersUpdateNeverRaisesAnAdministratorPrompt(t *testing.T) {
	called := stubElevation(t, nil)
	refuseSwap(t, false)

	err := replaceBinary(context.Background(), `C:\x\clawdh.exe.new`, `C:\x\clawdh.exe`)
	if err == nil {
		t.Fatal("a refused swap reported success")
	}
	if *called {
		t.Error("the automatic update raised an administrator prompt")
	}
	if !strings.Contains(err.Error(), "Update now") {
		t.Errorf("the message does not say how to approve it: %v", err)
	}
}

// An update a person asked for, into a folder that really needs administrator
// rights, is the one case that may prompt.
func TestAnUpdateAPersonAskedForMayAskForAdministrator(t *testing.T) {
	called := stubElevation(t, nil)
	refuseSwap(t, false)

	if err := replaceBinary(WithInteractive(context.Background()), `C:\x\clawdh.exe.new`, `C:\x\clawdh.exe`); err != nil {
		t.Fatalf("an approved elevated update failed: %v", err)
	}
	if !*called {
		t.Error("the elevated path was never reached for a folder that needs it")
	}
}

// Startup removes every displaced image nobody is running any more, under any of
// the names updates have used, and nothing else.
func TestCleanupOldBinaryRemovesEveryDisplacedImage(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "clawdh.exe")
	for _, name := range []string{"clawdh.exe", "clawdh.exe.old", "clawdh.exe.old-1700000000000", "clawdh.exe.old-1700000000001", "other.exe", "clawdh.exe.new"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	CleanupOldBinary(bin)

	for _, gone := range []string{"clawdh.exe.old", "clawdh.exe.old-1700000000000", "clawdh.exe.old-1700000000001"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s was left behind", gone)
		}
	}
	for _, kept := range []string{"clawdh.exe", "other.exe", "clawdh.exe.new"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}
