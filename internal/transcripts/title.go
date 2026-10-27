// Package transcripts reads what a person would call a Claude Code conversation
// from the transcript Claude Code keeps for it, without reading the transcript.
//
// A long session's transcript runs to hundreds of megabytes, and the page asks
// for every live session's name every few seconds. Claude Code re-appends the
// title records as a conversation goes on, so the end of the file almost always
// holds the current one; the start holds the first one and the first prompt.
// Only those two ends are read, and only again when the file has changed.
package transcripts

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ends is how much of each end of a transcript is read.
const ends = 4 << 20

// maxTitle is how long a title built from a prompt is allowed to run.
const maxTitle = 80

// SessionTitle is the title of the conversation sessionID in the user's own
// ~/.claude, or "" when there is no transcript for it yet (or nothing in it to
// name it by). A caller shows its own fallback, such as a short ID, for "".
func SessionTitle(sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return Title(filepath.Join(home, ".claude"), sessionID)
}

// Title is the title of the conversation sessionID under claudeDir: the name a
// person gave it, else the one Claude Code generated, else its first prompt.
func Title(claudeDir, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.ContainsAny(sessionID, `/\*?[`) {
		return ""
	}
	path := locate(claudeDir, sessionID)
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	mu.Lock()
	c, ok := cache[path]
	mu.Unlock()
	if ok && c.size == info.Size() && c.mod.Equal(info.ModTime()) {
		return c.title
	}
	title := readTitle(path, info.Size())
	mu.Lock()
	cache[path] = cached{size: info.Size(), mod: info.ModTime(), title: title}
	mu.Unlock()
	return title
}

type cached struct {
	size  int64
	mod   time.Time
	title string
}

var (
	mu    sync.Mutex
	cache = map[string]cached{}
	paths = map[string]string{} // claudeDir + session ID → transcript
)

// locate finds the transcript by its file name, as switching.HasTranscript
// does, rather than reproduce how Claude Code turns a folder into a project name.
func locate(claudeDir, sessionID string) string {
	key := claudeDir + "\x00" + sessionID
	mu.Lock()
	p := paths[key]
	mu.Unlock()
	if p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	matches, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", sessionID+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	mu.Lock()
	paths[key] = matches[0]
	mu.Unlock()
	return matches[0]
}

// record is the part of a transcript line a title can come from.
type record struct {
	Type        string `json:"type"`
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
	IsMeta      bool   `json:"isMeta"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func readTitle(path string, size int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := readAt(f, 0, min(size, ends))
	var tail []byte
	if size > ends {
		tail = readAt(f, size-ends, ends)
	}
	// The tail is later than the head, so what it says wins.
	custom, ai, _ := scan(tail)
	hCustom, hAI, prompt := scan(head)
	switch {
	case custom != "":
		return custom
	case hCustom != "":
		return hCustom
	case ai != "":
		return ai
	case hAI != "":
		return hAI
	}
	return prompt
}

func readAt(f *os.File, off, n int64) []byte {
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil
	}
	return buf[:got]
}

// scan is the latest custom and generated titles in a stretch of transcript, and
// its first prompt. A stretch cut from the middle of a file begins and may end
// mid-line; those partial lines simply fail to parse and are skipped.
func scan(b []byte) (custom, ai, prompt string) {
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		isTitle := bytes.Contains(line, []byte(`"custom-title"`)) || bytes.Contains(line, []byte(`"ai-title"`))
		isUser := prompt == "" && bytes.Contains(line, []byte(`"type":"user"`))
		if !isTitle && !isUser {
			continue
		}
		var r record
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		switch r.Type {
		case "custom-title":
			if t := clean(r.CustomTitle); t != "" {
				custom = t
			}
		case "ai-title":
			if t := clean(r.AITitle); t != "" {
				ai = t
			}
		case "user":
			if !r.IsMeta && r.Message.Role == "user" {
				prompt = promptText(r.Message.Content)
			}
		}
	}
	return
}

// promptText is what a person typed, from a user message's content (a string, or
// blocks of which the text ones count). Tool results and the bracketed notes
// Claude Code adds around commands are not a prompt.
func promptText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) != nil {
			return ""
		}
		for _, b := range blocks {
			if b.Type == "text" && !strings.HasPrefix(strings.TrimSpace(b.Text), "<") {
				s = b.Text
				break
			}
		}
	}
	if strings.HasPrefix(strings.TrimSpace(s), "<") {
		return ""
	}
	return clean(s)
}

// clean folds a title onto one line and keeps it to a readable length.
func clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxTitle {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:maxTitle-1])) + "…"
	}
	return s
}
