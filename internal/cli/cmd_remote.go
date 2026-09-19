package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"clawdh/internal/buildinfo"
	"clawdh/internal/claudebin"
	"clawdh/internal/config"
	"clawdh/internal/remotejobs"
	"clawdh/internal/service"
	"clawdh/internal/switching"
	"clawdh/panel"
)

// cmdRemote turns this machine's remote help on or off. Remote help lets the
// panel ask this machine to look at itself — diagnose a problem, list its
// sessions, send a transcript for debugging — and nothing of the sort can
// happen until the person here turns it on. Every request that does run is
// printed, so it is never silent.
func cmdRemote(args []string) int {
	_, _, clientPath, err := panelPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	cfg, err := panel.LoadClientConfig(clientPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}

	action := "status"
	if len(args) > 0 {
		action = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch action {
	case "on", "off":
		if !cfg.Configured() {
			fmt.Println("This machine isn't connected to a panel, so there's nothing to turn on.")
			fmt.Printf("Connect it first from the clawdh page (http://127.0.0.1:%d) or `clawdh join <link>`.\n", config.DefaultPort)
			return 1
		}
		cfg.Remote = action == "on"
		if err := panel.SaveClientConfig(clientPath, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "clawdh:", err)
			return 1
		}
		if cfg.Remote {
			fmt.Println("Remote help is on. The panel can now ask this machine to diagnose itself, list")
			fmt.Println("the sessions that ran through a shared account, or send one of their transcripts.")
			fmt.Println("Your own sessions (your personal login) are never visible to it. Every request is")
			fmt.Println("printed here and raises a notification.")
			fmt.Println("Turn it off any time with `clawdh remote off`.")
		} else {
			fmt.Println("Remote help is off. The panel can no longer ask this machine for anything.")
		}
		return 0
	case "status", "":
		if !cfg.Configured() {
			fmt.Println("This machine isn't connected to a panel.")
			return 0
		}
		if cfg.Remote {
			fmt.Println("Remote help is ON — the panel may ask this machine to diagnose itself, or list/send")
			fmt.Println("shared-account sessions only (never your own). Each request is printed. Turn off: `clawdh remote off`.")
		} else {
			fmt.Println("Remote help is OFF. Turn on with `clawdh remote on` to let the panel ask this")
			fmt.Println("machine to look at itself (diagnose, or list/send shared-account sessions only).")
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: clawdh remote [on|off|status]")
		return 2
	}
}

// wireRemoteQueue points the service's approval queue at its state file and
// gives it the two things it needs from here: how to run a request on this
// machine, and how to post the outcome back to the panel.
func wireRemoteQueue() {
	if path, err := config.RemoteRequestsFile(); err == nil {
		remotejobs.Default.SetPath(path)
	}
	remotejobs.Default.Run = func(kind, params string) (string, string) {
		return executeJob(panel.RemoteJob{Kind: kind, Params: params})
	}
	remotejobs.Default.Report = func(ctx context.Context, id, status, result string) error {
		c, err := panelClient()
		if err != nil {
			return err
		}
		return c.ReportResult(ctx, id, status, result)
	}
}

// needsApproval is whether a job ships something off this machine and so waits
// for the owner's decision. A health check reveals nothing personal — service
// state, the claude binary's path, how many accounts are shared — and runs at
// once; anything about sessions is the owner's call.
func needsApproval(kind string) bool { return kind != "diagnose" }

// runRemoteJobs takes the jobs a check-in handed back. A health check runs and
// answers at once. A request for sessions or a transcript is handed to the
// approval queue: inside an allow window it runs now, otherwise it is held —
// the panel sees "waiting for approval" — until the owner allows or denies it on
// their clawdh page. Either way the person is told, so nothing is silent.
func runRemoteJobs(ctx context.Context, c *panel.Client, jobs []panel.RemoteJob) {
	for _, j := range jobs {
		who := j.RequestedBy
		if who == "" {
			who = "the panel"
		}
		if !needsApproval(j.Kind) {
			fmt.Printf("clawdh: %s asked this machine to %s — running it.\n", who, describeJob(j))
			notifyBrief(who + " ran a health check on this machine")
			result, status := executeJob(j)
			if err := c.ReportResult(ctx, j.ID, status, result); err != nil {
				fmt.Fprintf(os.Stderr, "clawdh: could not send the result of %q back to the panel: %v\n", j.Kind, err)
			}
			continue
		}
		req := remotejobs.Request{ID: j.ID, Kind: j.Kind, Params: j.Params, RequestedBy: who, Describe: describeJob(j)}
		if remotejobs.Default.Receive(ctx, req) {
			fmt.Printf("clawdh: %s asked this machine to %s — waiting for you to allow or deny it on the clawdh page (http://127.0.0.1:%d).\n", who, describeJob(j), config.DefaultPort)
			notifyBrief(who + " asked to " + describeJob(j) + " — allow or deny it on your clawdh page")
		} else {
			fmt.Printf("clawdh: %s asked this machine to %s — ran it (you're allowing requests for now).\n", who, describeJob(j))
			notifyBrief(who + "'s request to " + describeJob(j) + " ran (you're allowing requests for now)")
		}
	}
}

func describeJob(j panel.RemoteJob) string {
	switch j.Kind {
	case "diagnose":
		return "check its own health"
	case "sessions":
		return "list the sessions that ran on a shared account"
	case "transcript":
		if j.Params != "" {
			return "send the transcript of shared-account session " + j.Params
		}
		return "send a shared-account session transcript"
	default:
		return j.Kind
	}
}

// executeJob runs one read-only job and returns its result text and a status
// ("done" or "error"). Nothing here changes the machine.
func executeJob(j panel.RemoteJob) (result, status string) {
	switch j.Kind {
	case "diagnose":
		return jobDiagnose(), "done"
	case "sessions":
		return jobSessions(), "done"
	case "transcript":
		out, err := jobTranscript(j.Params)
		if err != nil {
			return err.Error(), "error"
		}
		return out, "done"
	default:
		return "This machine doesn't know how to " + j.Kind + ".", "error"
	}
}

// claudeRootOverride lets tests point the Claude data dir at a temp dir. Empty
// in normal use.
var claudeRootOverride string

// claudeRoot is this machine's Claude Code data dir (~/.claude), where the
// session transcripts live. Remote help never browses it: it only reads the
// specific sessions the shared-session ledger says ran on a shared account.
func claudeRoot() (string, error) {
	if claudeRootOverride != "" {
		return claudeRootOverride, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// jobDiagnose reports whether clawdh is healthy here and can reach the gateway —
// the machine-side half of "why isn't it working for them?".
func jobDiagnose() string {
	var b strings.Builder
	fmt.Fprintf(&b, "clawdh %s on %s\n", buildinfo.Version, runtimeHost())

	if info, err := service.Running(); err == nil && info != nil {
		fmt.Fprintf(&b, "service: running (port %d, pid %d, version %s)\n", info.Port, info.PID, info.Version)
	} else {
		fmt.Fprintf(&b, "service: NOT running — `clawdh start` would bring it back\n")
	}

	if claude := claudebin.Resolve(); claude != "" {
		fmt.Fprintf(&b, "claude binary: %s\n", claude)
	} else {
		fmt.Fprintf(&b, "claude binary: not found on PATH\n")
	}

	shares := loadSharesForDiag()
	fmt.Fprintf(&b, "shared accounts: %d\n", len(shares))
	if len(shares) > 0 {
		gw := shares[0].Gateway
		fmt.Fprintf(&b, "gateway: %s — %s\n", gw, reachable(gw))
	} else {
		fmt.Fprintf(&b, "gateway: none configured (no accounts shared with this machine yet)\n")
	}
	return b.String()
}

// sharedLedgerOverride lets tests point the shared-session ledger at a temp
// file. Empty in normal use.
var sharedLedgerOverride string

func sharedLedgerPath() string {
	if sharedLedgerOverride != "" {
		return sharedLedgerOverride
	}
	p, _ := config.SharedSessionsFile()
	return p
}

// sharedSessionOut is one row of a sessions answer: what the panel is allowed
// to know about a session — its id, which shared account it ran on, the project
// folder name, size and time. Never its content.
type sharedSessionOut struct {
	ID      string `json:"id"`
	Share   string `json:"share"`
	Project string `json:"project"`
	Size    int64  `json:"size"`
	Mod     string `json:"mod"`
}

// jobSessions lists the sessions on this machine that ran through a shared
// account — and only those, read off clawdh's shared-session ledger. A person's
// own sessions (their personal login, or a local account they manage
// themselves) are not the panel's concern and are never listed, whatever else
// is on the disk. The answer is JSON the panel renders.
func jobSessions() string {
	shared := switching.SharedSessions(sharedLedgerPath())
	var files []sessionFile
	for _, f := range sessionFiles() {
		if slug, ok := shared[f.id]; ok {
			f.share = slug
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > 50 {
		files = files[:50]
	}
	out := struct {
		Sessions []sharedSessionOut `json:"sessions"`
		Note     string             `json:"note"`
	}{Note: "Only sessions that ran through a shared account are listed; a person's own sessions are never shown."}
	for _, f := range files {
		out.Sessions = append(out.Sessions, sharedSessionOut{ID: f.id, Share: f.share, Project: f.project, Size: f.size, Mod: f.mod.Format("2006-01-02 15:04")})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// maxTranscriptBytes caps a returned transcript, so a giant session can't be
// dragged whole through the panel; the tail is what a recent problem is in.
const maxTranscriptBytes = 512 << 10

// jobTranscript returns one named session's transcript (its tail, if large).
func jobTranscript(sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", fmt.Errorf("no session id was given")
	}
	// The id is a file stem; keep it to a single path element so it can't escape
	// the projects tree.
	if strings.ContainsAny(sessionID, "/\\") || strings.Contains(sessionID, "..") {
		return "", fmt.Errorf("that is not a valid session id")
	}
	// Only a session that ran through a shared account is the panel's to see —
	// the ledger, not the disk, decides. Everything else stays private.
	if _, ok := switching.SharedSessions(sharedLedgerPath())[sessionID]; !ok {
		return "", fmt.Errorf("session %q did not run through a shared account, so it isn't the panel's to see", sessionID)
	}
	matches, _ := filepath.Glob(filepath.Join(claudeProjectsDir(), "*", sessionID+".jsonl"))
	if len(matches) == 0 {
		return "", fmt.Errorf("no session %q on this machine", sessionID)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		return "", fmt.Errorf("could not read that session: %w", err)
	}
	if len(data) > maxTranscriptBytes {
		return "…(truncated to the last " + humanSize(maxTranscriptBytes) + ")\n" + string(data[len(data)-maxTranscriptBytes:]), nil
	}
	return string(data), nil
}

// --- small helpers ---------------------------------------------------------

type sessionFile struct {
	id, project string
	share       string // the shared account it ran on, from the ledger
	size        int64
	mod         time.Time
}

func claudeProjectsDir() string {
	root, err := claudeRoot()
	if err != nil {
		return ""
	}
	return filepath.Join(root, "projects")
}

func sessionFiles() []sessionFile {
	root := claudeProjectsDir()
	matches, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	var out []sessionFile
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		out = append(out, sessionFile{
			id:      strings.TrimSuffix(filepath.Base(m), ".jsonl"),
			project: filepath.Base(filepath.Dir(m)),
			size:    info.Size(),
			mod:     info.ModTime(),
		})
	}
	return out
}

func loadSharesForDiag() []panel.GatewayShare {
	path, err := config.SharesFile()
	if err != nil {
		return nil
	}
	shares, _ := panel.LoadShares(path)
	return shares
}

// reachable reports, in words, whether the gateway answers — any HTTP response
// (even a 401) means the network path is good; only a transport error is a
// problem the person needs to know about.
func reachable(url string) string {
	if url == "" {
		return "no URL"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "UNREACHABLE (" + err.Error() + ")"
	}
	defer resp.Body.Close()
	return fmt.Sprintf("reachable (HTTP %d)", resp.StatusCode)
}

func runtimeHost() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "this machine"
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
