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
	// Version is the clawdh build the machine last checked in with, as its own
	// page shows it ("main · 7b506ea"), so the panel can say which machines are
	// behind. "" until a build that reports it checks in.
	Version string `json:"version,omitempty"`
	// UpdateError is why this machine's last attempt to update itself failed,
	// empty when it did not. A machine that cannot install a release is
	// otherwise indistinguishable from one nobody has superseded — both just
	// sit on an old build — and the reason only ever appeared in a log file on
	// the machine itself, which is the one place nobody looks.
	UpdateError string `json:"updateError,omitempty"`
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
	// AddedBy is the person whose machine handed the login up — the one who may
	// take it back again without the admin. "" for an account from before this
	// was recorded; only the admin removes those.
	AddedBy string `json:"addedBy,omitempty"`
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
	// ExpiresAt, when set, is when this access ends on its own — "for the
	// afternoon", "for the week" — so lending an account across teams never
	// depends on someone remembering to take it back. Zero means until revoked.
	// GrantedBy is who gave it, so the line that records it running out can say
	// whose call it was.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	GrantedBy string    `json:"grantedBy,omitempty"`
}

// Live reports whether a share still grants access at now: it has no
// deadline, or the deadline hasn't passed. The gateway and every listing go
// through this, so an expired share stops working the second its time is up,
// whether or not the sweep has removed it yet.
func (sh Share) Live(now time.Time) bool { return sh.ExpiresAt.IsZero() || now.Before(sh.ExpiresAt) }

// JoinCode is the one-shot code inside an invite link: it enrols a machine as
// a given person. InvitedBy is the signer who made the invite, so the page the
// link opens can say who is inviting.
type JoinCode struct {
	CodeHash  string    `json:"codeHash"`
	PersonID  string    `json:"personId"`
	ExpiresAt time.Time `json:"expiresAt"`
	UsedAt    time.Time `json:"usedAt,omitempty"`
	InvitedBy string    `json:"invitedBy,omitempty"`
}

// Admin is the single administrator's password, salted and stretched.
type Admin struct {
	Salt []byte `json:"salt"`
	Hash []byte `json:"hash"`
}
