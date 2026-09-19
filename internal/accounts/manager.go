package accounts

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Manager is the CRUD API the HTTP layer drives. It owns slug/alias
// uniqueness and per-account directory provisioning; it knows nothing
// about PTYs, shells, or services.
type Manager struct {
	store       *Store
	accountsDir string
}

// NewManager builds a Manager whose account directories live under
// accountsDir (typically ~/.clawdh/accounts).
func NewManager(store *Store, accountsDir string) *Manager {
	return &Manager{store: store, accountsDir: accountsDir}
}

// List returns every known account.
func (m *Manager) List() ([]Account, error) {
	list, err := m.store.Load()
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i] = withLoginState(list[i])
	}
	return list, nil
}

// withLoginState corrects a stored status against the account's actual
// credential store.
//
// "linked" is recorded when a login is observed to succeed, and then never
// looked at again — so an account whose credentials have since gone stays
// "linked" for ever, and every reader believes it. That is not academic: both
// managed accounts on the author's machine had their stored logins destroyed by
// a bug elsewhere in clawdh and went on being reported as linked, right up to the
// point a session failed to authenticate.
//
// Only the downgrade is done here. Saying "linked" because a store happens to
// hold something would be the same mistake in the other direction: a login can
// be present and expired, and only Claude Code can settle that.
func withLoginState(a Account) Account {
	if a.Status != StatusLinked {
		return a
	}
	// Downgrade a stale "linked" when the login is actually gone. This reads
	// whether a credential exists (a file, or on macOS the Keychain item) —
	// never whether it is still valid, which only Claude Code can settle. An
	// expired-but-present login stays "linked"; the live status beside it
	// reports expiry separately.
	if _, err := CaptureLogin(a.ConfigDir); err != nil {
		a.Status = StatusPending
	}
	return a
}

// Get returns a single account by ID, or an error if it doesn't exist.
func (m *Manager) Get(id string) (Account, error) {
	list, err := m.store.Load()
	if err != nil {
		return Account{}, err
	}
	for _, a := range list {
		if a.ID == id {
			return withLoginState(a), nil
		}
	}
	return Account{}, fmt.Errorf("no account with id %q", id)
}

// Add provisions a new account: a unique slug/ID/alias derived from name,
// a fresh (empty) config directory, and status "pending" until a login
// succeeds against it.
func (m *Manager) Add(name string) (Account, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Account{}, fmt.Errorf("account name must not be empty")
	}

	var created Account
	_, err := m.store.Mutate(func(list []Account) ([]Account, error) {
		// Create the directory rather than accepting an existing one: a
		// leftover from a removal that failed (routine on Windows, where
		// an open handle blocks deletion) would otherwise be adopted
		// silently, and the "new" account would already be signed in as
		// the identity the user thought they had deleted.
		base := slugify(name)
		slug := uniqueSlug(base, list)
		var dir string
		for attempt := 0; ; attempt++ {
			dir = filepath.Join(m.accountsDir, slug)
			if err := os.MkdirAll(m.accountsDir, 0o700); err != nil {
				return nil, fmt.Errorf("creating accounts dir: %w", err)
			}
			err := os.Mkdir(dir, 0o700)
			if err == nil {
				break
			}
			if !os.IsExist(err) {
				return nil, fmt.Errorf("creating account dir: %w", err)
			}
			if attempt > 50 {
				return nil, fmt.Errorf("creating account dir: %s already exists", dir)
			}
			slug = fmt.Sprintf("%s-%d", base, attempt+2)
			slug = uniqueSlug(slug, list)
		}
		created = Account{
			ID:        slug,
			Name:      name,
			Slug:      slug,
			Kind:      KindManaged,
			ConfigDir: dir,
			Isolation: IsolationCredentialsOnly,
			Alias:     uniqueAlias(aliasFor(slug), list),
			Status:    StatusPending,
			CreatedAt: time.Now().UTC(),
		}
		return append(list, created), nil
	})
	if err != nil {
		return Account{}, err
	}
	return created, nil
}

// Rename changes an account's display name and, since the alias is
// derived from the name, regenerates its alias. The account's ID, slug,
// and config directory never change, so its credentials are untouched.
func (m *Manager) Rename(id, newName string) (Account, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return Account{}, fmt.Errorf("account name must not be empty")
	}

	var updated Account
	found := false
	_, err := m.store.Mutate(func(list []Account) ([]Account, error) {
		for i := range list {
			if list[i].ID == id {
				others := append(append([]Account{}, list[:i]...), list[i+1:]...)
				list[i].Name = newName
				list[i].Alias = uniqueAlias(aliasFor(slugify(newName)), others)
				updated = list[i]
				found = true
				return list, nil
			}
		}
		return list, nil
	})
	if err != nil {
		return Account{}, err
	}
	if !found {
		return Account{}, fmt.Errorf("no account with id %q", id)
	}
	return updated, nil
}

// SetStatus updates an account's lifecycle status, e.g. once a login is
// observed to succeed.
func (m *Manager) SetStatus(id string, status Status) (Account, error) {
	var updated Account
	found := false
	_, err := m.store.Mutate(func(list []Account) ([]Account, error) {
		for i := range list {
			if list[i].ID == id {
				list[i].Status = status
				if status == StatusLinked {
					list[i].LastUsedAt = time.Now().UTC()
				}
				updated = list[i]
				found = true
				return list, nil
			}
		}
		return list, nil
	})
	if err != nil {
		return Account{}, err
	}
	if !found {
		return Account{}, fmt.Errorf("no account with id %q", id)
	}
	return updated, nil
}

// SetPanelID records that this account was lent by a clawdh panel, so a later
// check-in can tell it apart from one the user made themselves — which the
// panel must never take away.
func (m *Manager) SetPanelID(id, panelID string) (Account, error) {
	var updated Account
	found := false
	_, err := m.store.Mutate(func(list []Account) ([]Account, error) {
		for i := range list {
			if list[i].ID == id {
				list[i].PanelID = panelID
				updated = list[i]
				found = true
				return list, nil
			}
		}
		return list, nil
	})
	if err != nil {
		return Account{}, err
	}
	if !found {
		return Account{}, fmt.Errorf("no account with id %q", id)
	}
	return updated, nil
}

// Remove deletes an account's metadata and its on-disk config directory,
// and returns the removed record so the caller (the HTTP layer) can also
// strip its alias from shell rc files.
func (m *Manager) Remove(id string) (Account, error) {
	var removed Account
	found := false
	_, err := m.store.Mutate(func(list []Account) ([]Account, error) {
		out := make([]Account, 0, len(list))
		for _, a := range list {
			if a.ID == id {
				removed = a
				found = true
				continue
			}
			out = append(out, a)
		}
		return out, nil
	})
	if err != nil {
		return Account{}, err
	}
	if !found {
		return Account{}, fmt.Errorf("no account with id %q", id)
	}
	if removed.IsDefault() {
		// Removing the adopted default account only makes clawdh forget
		// it. Its directory is the user's main Claude Code login.
		if err := m.store.SetDefaultDismissed(true); err != nil {
			return removed, err
		}
		return removed, nil
	}

	if removed.OwnsConfigDir() {
		if err := os.RemoveAll(removed.ConfigDir); err != nil && !os.IsNotExist(err) {
			// Reporting success here is how a later Add lands on a
			// directory that still holds the old credentials.
			return removed, fmt.Errorf("account removed, but its directory %s could not be deleted: %w", removed.ConfigDir, err)
		}
	}
	return removed, nil
}

var nonSlugChars = regexp.MustCompile(`[^a-z0-9]+`)

// slugify is the short name typed at the terminal for an account (`clawdh
// <slug>`). A login is often named by its email, and nobody wants to type
// "ehtisham-devhouse-co" — so an email-shaped name shortens to the part before
// the @ (the domain adds nothing once the name is unique here).
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if at := strings.IndexByte(s, '@'); at > 0 && !strings.ContainsAny(s[:at], " ") {
		if local := strings.Trim(nonSlugChars.ReplaceAllString(s[:at], "-"), "-"); local != "" {
			s = local
		}
	}
	s = nonSlugChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "account"
	}
	return s
}

func aliasFor(slug string) string {
	return "claude-" + slug
}

func uniqueSlug(base string, existing []Account) string {
	candidate := base
	for i := 2; slugTaken(candidate, existing); i++ {
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return candidate
}

func slugTaken(slug string, existing []Account) bool {
	for _, a := range existing {
		if a.Slug == slug || a.ID == slug {
			return true
		}
	}
	return false
}

func uniqueAlias(base string, existing []Account) string {
	candidate := base
	for i := 2; aliasTaken(candidate, existing); i++ {
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return candidate
}

func aliasTaken(alias string, existing []Account) bool {
	for _, a := range existing {
		if a.Alias == alias {
			return true
		}
	}
	return false
}
