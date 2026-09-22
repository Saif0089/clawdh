// Package sessions is the register of Claude Code sessions clawdh is
// supervising on this machine, and the one account new ones start as.
//
// Everything here exists to answer two questions a person asks out loud and
// could not previously ask the page: *what am I running right now, and as
// whom*, and *how do I move this conversation to another account*.
//
// The second was always possible and never discoverable. A supervised session
// watches a handoff file while Claude Code runs, and relaunches itself —
// same conversation, same window — on whichever account that file names. The
// only thing that ever wrote one was the hook behind `clawdh <name>` typed at
// the prompt, which you had to know about. So a supervisor now publishes
// itself here, and anything that can write a handoff — the local page, most of
// all — can move a running conversation without the person knowing the
// command, or which of their windows is a terminal and which is an editor.
//
// A register entry is a fact about a process, not a secret: a pid, the
// conversation's id, the account it is on, where it was started. No token, no
// credential, nothing about what is being said. It lives under ~/.clawdh and
// is read only by this machine's own clawdh.
package sessions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Host is where a session is being typed into.
type Host string

const (
	// HostTerminal is a session someone started in a terminal, with `clawdh
	// <name>`, `clawdh shared <name>`, or a plain `claude` through the shell
	// function.
	HostTerminal Host = "terminal"
	// HostEditor is one chat in a VS Code-family editor, launched by the Claude
	// Code extension through clawdh's wrapper.
	HostEditor Host = "editor"
)

// Session is one live supervised Claude Code session.
type Session struct {
	PID int `json:"pid"`
	// Nonce is minted once per supervisor and copied into any handoff staged
	// for it, so a switch aimed at a session that has since died cannot be
	// picked up by an unrelated supervisor that happened to inherit its pid.
	Nonce string `json:"nonce"`
	// SessionID is the conversation, so a switch resumes this one and not
	// merely the most recent in the directory.
	SessionID string `json:"sessionId,omitempty"`
	// Account is the display name of what it is running as right now — it
	// changes when the session is switched.
	Account string `json:"account"`
	// AccountID is set for a local account, Slug for a gateway share; exactly
	// one of them, which is also how a reader tells the two apart.
	AccountID string `json:"accountId,omitempty"`
	Slug      string `json:"slug,omitempty"`
	Shared    bool   `json:"shared,omitempty"`

	Host   Host   `json:"host"`
	Editor string `json:"editor,omitempty"` // which one, when it could be told
	Dir    string `json:"dir,omitempty"`    // the folder it was started in

	// Handoff is the file this supervisor watches for a staged switch.
	Handoff   string    `json:"handoff"`
	StartedAt time.Time `json:"startedAt"`
}

// NewNonce mints a supervisor's nonce. A failure to read randomness is not a
// reason to refuse to run a session: the nonce then stays empty, which simply
// means this session accepts any handoff aimed at it, exactly as before.
func NewNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// Publish records a live session, replacing whatever this pid published
// before — a switch rewrites the entry so the register always names the
// account the session is on now.
func Publish(dir string, s Session) error {
	if dir == "" || s.PID <= 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	path := pathFor(dir, s.PID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Withdraw removes a session from the register. A supervisor does this on its
// way out; a missing file is not an error.
func Withdraw(dir string, pid int) {
	if dir == "" || pid <= 0 {
		return
	}
	os.Remove(pathFor(dir, pid))
	os.Remove(pathFor(dir, pid) + ".tmp")
}

// List is every session still running, oldest first. Entries whose process is
// gone are removed as they are found: a supervisor killed outright never got
// to withdraw itself, and a register that keeps counting the dead is worse
// than no register at all.
func List(dir string) []Session {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s Session
		if err := json.Unmarshal(data, &s); err != nil || s.PID <= 0 {
			os.Remove(path)
			continue
		}
		if !Alive(s.PID) {
			os.Remove(path)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].PID < out[j].PID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// Get returns one live session by pid.
func Get(dir string, pid int) (Session, bool) {
	for _, s := range List(dir) {
		if s.PID == pid {
			return s, true
		}
	}
	return Session{}, false
}

func pathFor(dir string, pid int) string {
	return filepath.Join(dir, strconv.Itoa(pid)+".json")
}
