package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"clawdh/internal/buildinfo"
	"clawdh/internal/service"
	"clawdh/internal/updater"
)

// cmdUpdate checks for a published release and installs it, now, instead of
// waiting for the service's own timer.
//
// The timer is the ordinary path and it works — when the service is running.
// The case this command is for is the one where it was not: a machine that
// booted without clawdh coming up updates exactly never, and until now the
// only way out was to reinstall. So this works either way. With a service up,
// it asks that service to do it, because the service owns the binary and can
// restart into the new one. With no service, it does the work here.
//
//	clawdh update           check, and install anything newer
//	clawdh update --check   say what is published, install nothing
func cmdUpdate(args []string) int {
	checkOnly := len(args) > 0 && (args[0] == "--check" || args[0] == "-n")

	fmt.Printf("This build: %s\n", buildinfo.Tag())
	if !updater.Enabled() {
		fmt.Fprintln(os.Stderr, "clawdh: automatic updates are switched off here (CLAWDH_AUTO_UPDATE=0).")
		fmt.Fprintln(os.Stderr, "      Unset that to let clawdh update itself, or reinstall to move builds by hand.")
		return 1
	}

	binaryPath, err := service.SelfPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh: cannot find my own binary:", err)
		return 1
	}
	up := updater.New(binaryPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if checkOnly {
		release, err := up.Latest(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "clawdh:", updateProblem(err))
			return 1
		}
		fmt.Printf("Published:  %s (%s)\n", release.Name, release.PublishedAt.Local().Format("Jan 2, 3:04 pm"))
		fmt.Println("\nRun `clawdh update` to install it if it is newer than this build.")
		return 0
	}

	// A running service is the one that should do this: it holds the binary
	// open and knows how to restart into the replacement.
	if info, _ := service.Running(); info != nil {
		outcome, err := askServiceToUpdate(ctx, info.Port)
		if err == nil {
			fmt.Println(outcome)
			return 0
		}
		// Falling through is right: the service may be a build too old to
		// have this endpoint, or wedged. Say so, then do it here.
		fmt.Fprintln(os.Stderr, "clawdh: the running service could not do it ("+err.Error()+"); updating directly.")
	}

	release, err := up.CheckAndApply(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", updateProblem(err))
		return 1
	}
	if release == nil {
		fmt.Println("Already on the newest build; nothing to do.")
		return 0
	}
	fmt.Printf("Updated to %s.\n", release.Name)
	// The binary on disk is new, but whatever is running is still the old
	// image. Restarting the service is what makes the new build the one
	// answering on the page.
	if info, _ := service.Running(); info != nil {
		svc := service.New(binaryPath, info.Port)
		if err := svc.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "clawdh: could not stop the old service:", err)
			return 1
		}
		if err := svc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "clawdh: could not start the new build:", err)
			return 1
		}
		fmt.Println("The service is now running the new build.")
	}
	return 0
}

// askServiceToUpdate has the running service do the check, and returns what it
// said.
func askServiceToUpdate(ctx context.Context, port int) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/api/update", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		if out.Error != "" {
			return "", fmt.Errorf("%s", out.Error)
		}
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out.Message == "" {
		return "", fmt.Errorf("the service gave no answer")
	}
	return out.Message, nil
}

// updateProblem words a failed check for someone reading a terminal. Being
// rate-limited is not a broken updater and must not read like one.
func updateProblem(err error) string {
	if err == nil {
		return ""
	}
	if isRateLimited(err) {
		return "GitHub is rate-limiting update checks from this network right now. " +
			"That limit is per address, so several machines behind one router share it. Try again in a few minutes."
	}
	return "could not check for updates: " + err.Error()
}

func isRateLimited(err error) bool {
	return errors.Is(err, updater.ErrRateLimited)
}
