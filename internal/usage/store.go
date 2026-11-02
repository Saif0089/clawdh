package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"clawdh/internal/config"
)

// StaleReportAge is how long a saved report is worth showing.
//
// This was set when the shortest window on the page was the five-hour
// session allowance, so anything older than six hours had certainly rolled
// over. That window is gone, and a weekly figure only grows, so an old one is
// an under-count rather than a wrong one; six hours stays as the line past
// which an empty card is the more honest answer.
const StaleReportAge = 6 * time.Hour

// tooOld reports whether a report is missing or past StaleReportAge.
// The same rule decides what is read back from disk and what is served
// from memory.
func tooOld(report *Report, now time.Time) bool {
	return report == nil || now.Sub(report.FetchedAt) > StaleReportAge
}

// savedReports is the on-disk shape: account id -> the last report read
// for it. Numbers only; no part of a credential is ever written here.
type savedReports struct {
	Accounts map[string]*Report `json:"accounts"`
}

// loadReports fills lastReport from disk, dropping anything too old to
// mean what it says.
func (s *Service) loadReports() {
	path := s.cachePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // no file yet, or unreadable: start with nothing
	}
	var saved savedReports
	if err := json.Unmarshal(data, &saved); err != nil {
		return
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, report := range saved.Accounts {
		if tooOld(report, now) {
			continue
		}
		// A report saved before the five-hour window was removed still carries
		// it; it must not come back on the card for the hours it stays fresh.
		report.Limits = withoutSession(report.Limits)
		s.lastReport[id] = report
	}
}

// saveReports writes what is currently known. Called after a successful
// read and after Forget, both rare enough that a whole-file rewrite is
// simpler than anything cleverer.
//
// Failures are silent by design: this is a convenience for the next
// start, and nothing on the page depends on it having worked.
func (s *Service) saveReports() {
	path := s.cachePath()
	if path == "" {
		return
	}

	s.mu.Lock()
	saved := savedReports{Accounts: make(map[string]*Report, len(s.lastReport))}
	for id, report := range s.lastReport {
		saved.Accounts[id] = report
	}
	s.mu.Unlock()

	data, err := json.Marshal(saved)
	if err != nil {
		return
	}
	if err := config.EnsureDir(filepath.Dir(path)); err != nil {
		return
	}
	// Written to one side and renamed: a half-written file here would
	// be read back as no file at all on the next start, which is a
	// worse answer than the previous one.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".usage-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
	}
}

// cachePath is where reports are kept. Empty means persistence is off,
// which is the default: only NewService, the constructor the running
// server uses, points this at ~/.clawdh.
func (s *Service) cachePath() string {
	return s.CachePath
}
