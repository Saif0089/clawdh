package cli

import (
	"flag"
	"fmt"
	"os"
	"time"

	"clawdh/internal/config"
	"clawdh/internal/service"
)

func cmdStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	port := fs.Int("port", config.DefaultPort, "port to listen on")
	// The start-at-login entry passes this: it has nobody to report to, and its
	// own console window stays open for as long as this command runs.
	noWait := fs.Bool("no-wait", false, "return as soon as the server has been launched, without waiting for it to answer")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	binaryPath, err := resolveBinaryPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}

	// If clawdh is already up on a different port than the one asked
	// for, say so rather than printing a URL nothing is listening on.
	if info, _ := service.Running(); info != nil && info.Port != *port {
		fmt.Printf("clawdh is already running on http://127.0.0.1:%d (run `clawdh stop` first to move it to %d)\n", info.Port, *port)
		return 0
	}

	svc := service.New(binaryPath, *port)
	if err := svc.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	if *noWait {
		return 0
	}
	if info := waitForRunning(15 * time.Second); info != nil {
		fmt.Printf("clawdh is running: http://127.0.0.1:%d\n", info.Port)
		return 0
	}

	fmt.Fprintln(os.Stderr, "clawdh: started, but nothing is answering.")
	if reason := lastLogLine(); reason != "" {
		fmt.Fprintln(os.Stderr, "  last log line:", reason)
	}
	fmt.Fprintln(os.Stderr, "  full log: ~/.clawdh/clawdh.log")
	return 1
}

func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	port := fs.Int("port", config.DefaultPort, "port clawdh was running on")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	binaryPath, err := resolveBinaryPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	svc := service.New(binaryPath, *port)
	if err := svc.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	fmt.Println("Stopped.")
	return 0
}

func cmdStatus(args []string) int {
	info, err := service.Running()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	if info == nil {
		fmt.Println("clawdh is not running.")
		return 1
	}
	fmt.Printf("clawdh %s is running: http://127.0.0.1:%d\n", info.Version, info.Port)
	return 0
}
