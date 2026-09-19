// Package panel is clawdh's account-lending server: the small admin panel a
// person runs so the Claude logins a group shares can be handed out and taken
// back, instead of living on somebody's laptop for ever.
//
// Flat means flat. One admin, no teams, no roles, no organisations. The people
// it tracks are names attached to machines.
//
// One login serves many people at once through the gateway, which holds the
// login and refreshes it centrally. That is the whole reason the gateway
// exists: Claude Code rotates its refresh token on every renewal, so two
// machines holding one login would invalidate each other — the mechanism that
// once merged three of this project's accounts into a single broken login. With
// the gateway, members never hold the login at all; they hold a scoped key the
// gateway maps back to it.
package panel

import "time"

// Person is someone an account can be lent to.
type Person struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Device is one machine a person has enrolled. Each holds its own token, so a
// lost laptop can be cut off without disturbing the person's other machines.
type Device struct {
	ID         string    `json:"id"`
	PersonID   string    `json:"personId"`
	Name       string    `json:"name"`
	TokenHash  string    `json:"tokenHash"`
	EnrolledAt time.Time `json:"enrolledAt"`
	LastSeen   time.Time `json:"lastSeen,omitempty"`
	// Remote is whether this machine's owner turned remote help on — the consent
	// the jobs channel needs. The machine reports it every check-in; false (the
	// default) means the panel will not offer to ask this machine anything.
	Remote bool `json:"remote,omitempty"`
}

// Account is one Claude login the panel lends out.
//
// Credential is the sealed login, present only once it has been signed in
// through the panel. It is sealed with the panel's own key (see keyfile.go) so
// that a copy of panel.json on its own is not a set of working logins.
type Account struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Email      string    `json:"email,omitempty"`
	Plan       string    `json:"plan,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	Credential []byte    `json:"credential,omitempty"`
}

// HasLogin reports whether this account has a login to lend.
func (a Account) HasLogin() bool { return len(a.Credential) > 0 }

// Event is one line of the activity log.
//
// Who is the name the person signed in with. The panel is flat — one shared
// password, no accounts or roles — but several team leads share it, so every
// change is recorded under the name its author gave at sign-in: "Hassan gave
// Ibrahim access to Work", never an ambiguous "You". A machine pushing a login
// is recorded under its member name.
type Event struct {
	At   time.Time `json:"at"`
	Who  string    `json:"who"`
	What string    `json:"what"`
}

// Share is one account made available to one person through the gateway. Unlike
// the old one-holder assignment, an account can have many shares at once — that
// is the whole point of the gateway: many people, one login, together. Each
// share carries the hash of a gateway key the person's Claude Code presents; the
// gateway maps that key to this account's live token.
type Share struct {
	ID        string `json:"id"`
	AccountID string `json:"accountId"`
	PersonID  string `json:"personId"`
	KeyHash   string `json:"keyHash"`
	// SealedKey is the gateway key, sealed with the panel key, so an enrolled
	// device can be handed it back on check-in. The gateway matches by KeyHash;
	// this is only for delivery. A gateway key is a scoped bearer token, not the
	// Claude credential, so this is a lower-stakes secret than the login itself.
	SealedKey []byte    `json:"sealedKey,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// JoinCode is a one-shot code that enrols a machine as a given person.
type JoinCode struct {
	CodeHash  string    `json:"codeHash"`
	PersonID  string    `json:"personId"`
	ExpiresAt time.Time `json:"expiresAt"`
	UsedAt    time.Time `json:"usedAt,omitempty"`
}

// Admin is the single administrator's password, salted and stretched.
type Admin struct {
	Salt []byte `json:"salt"`
	Hash []byte `json:"hash"`
}
