package panel

import (
	"clawdh/internal/config"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// inviteLife is how long an invite link works. Short on purpose: an invite is
// sent through chat and sits in a history, so it stops being useful before it
// stops being findable. Long enough that a person can act on it in one sitting.
const inviteLife = 1 * time.Hour

// ---------------------------------------------------------------- the panel

type accountView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Plan     string `json:"plan,omitempty"`
	HasLogin bool   `json:"hasLogin"`
	// Warning is set when this account's shared login recently failed to refresh
	// — a sign it is being used first-party outside the gateway.
	Warning string `json:"warning,omitempty"`
	// Shared is everyone with gateway access to this account right now — many
	// people can share one login, so this is a list, not one holder.
	Shared []shareView `json:"shared,omitempty"`
	// AddedBy names the person whose machine handed the login up; they can take
	// it back from their own page. "" for an account from before that was kept.
	AddedBy string `json:"addedBy,omitempty"`
}

// shareView is one person's gateway access to an account, with the share id
// needed to take it away.
type shareView struct {
	ShareID    string    `json:"shareId"`
	PersonID   string    `json:"personId"`
	PersonName string    `json:"personName"`
	ExpiresAt  time.Time `json:"expiresAt,omitzero"` // zero: until revoked
}

type deviceView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
	Remote   bool       `json:"remote,omitempty"`
	Version  string     `json:"version,omitempty"` // the clawdh build it last checked in with
}

type personView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	// Can is the accounts this person may use through the gateway.
	Can []string `json:"can,omitempty"`
	// CanUntil is, per Can entry, when that access ends ("" = until revoked).
	CanUntil []string     `json:"canUntil,omitempty"`
	Devices  []deviceView `json:"devices,omitempty"`
}

// handlePanel builds the whole interface in one read. Three tabs over one
// answer is far less to keep in step than three endpoints that can disagree.
func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
	// A read that settles expiry first, so the panel never shows an account as
	// out when its time has already run out.
	var d Data
	if err := s.store.Mutate(func(cur *Data) error { d = *cur; return nil }); err != nil {
		fail(w, 500, err.Error())
		return
	}

	accounts := make([]accountView, 0, len(d.Accounts))
	for _, a := range d.Accounts {
		v := accountView{ID: a.ID, Name: a.Name, Email: a.Email, Plan: a.Plan, HasLogin: a.HasLogin(), AddedBy: d.personName(a.AddedBy)}
		for _, sh := range d.Shares {
			if sh.AccountID == a.ID {
				v.Shared = append(v.Shared, shareView{ShareID: sh.ID, PersonID: sh.PersonID, PersonName: d.personName(sh.PersonID), ExpiresAt: sh.ExpiresAt})
			}
		}
		accounts = append(accounts, v)
	}

	// Flag any account whose shared login broke in the last day — a sign it is
	// being used first-party, outside the gateway.
	if s.usage != nil {
		if col, err := s.usage.RecentCollisions(r.Context(), s.now().Add(-24*time.Hour)); err == nil {
			for i := range accounts {
				if _, hit := col[accounts[i].ID]; hit {
					accounts[i].Warning = "Its shared login stopped working in the last day — most likely the same account was used directly on another machine, which kills the copy the gateway holds. Re-add the login from that machine's clawdh page."
				}
			}
		}
	}

	people := make([]personView, 0, len(d.People))
	for _, p := range d.People {
		v := personView{ID: p.ID, Name: p.Name, Email: p.Email}
		for _, sh := range d.Shares {
			if sh.PersonID == p.ID {
				v.Can = append(v.Can, d.accountName(sh.AccountID))
				until := ""
				if !sh.ExpiresAt.IsZero() {
					until = sh.ExpiresAt.UTC().Format(time.RFC3339)
				}
				v.CanUntil = append(v.CanUntil, until)
			}
		}
		for _, dev := range d.Devices {
			if dev.PersonID != p.ID {
				continue
			}
			dv := deviceView{ID: dev.ID, Name: dev.Name, Remote: dev.Remote, Version: dev.Version}
			if !dev.LastSeen.IsZero() {
				seen := dev.LastSeen
				dv.LastSeen = &seen
			}
			v.Devices = append(v.Devices, dv)
		}
		people = append(people, v)
	}

	activity := append([]Event(nil), d.Activity...)
	sort.Slice(activity, func(i, j int) bool { return activity[i].At.After(activity[j].At) })
	if len(activity) > 200 {
		activity = activity[:200]
	}

	writeJSON(w, 200, map[string]any{"accounts": accounts, "people": people, "activity": activity})
}

// ---------------------------------------------------------------- accounts

// removeAccount drops an account and every share on it — everyone using it
// loses access, since dropping the shares stops their keys at the gateway on
// the next request. It is the one removal, whoever asked for it.
func removeAccount(d *Data, id string) (Account, bool) {
	a, ok := d.Account(id)
	if !ok {
		return Account{}, false
	}
	gone := *a
	shares := d.Shares[:0]
	for _, sh := range d.Shares {
		if sh.AccountID != id {
			shares = append(shares, sh)
		}
	}
	d.Shares = shares
	out := d.Accounts[:0]
	for _, acct := range d.Accounts {
		if acct.ID != id {
			out = append(out, acct)
		}
	}
	d.Accounts = out
	return gone, true
}

func (s *Server) handleRemoveAccount(w http.ResponseWriter, r *http.Request) {
	who := s.actor(r) // the signed-in name every change is recorded under
	id := r.PathValue("id")
	err := s.store.Mutate(func(d *Data) error {
		a, ok := removeAccount(d, id)
		if !ok {
			return errors.New("There is no such account.")
		}
		d.Log(s.now(), who, "removed "+a.Name)
		return nil
	})
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	w.WriteHeader(204)
}

// handleContribute is a member's machine handing up the login of an account
// signed in there, so it can be shared through the gateway. It is the one way a
// login reaches the panel — the panel cannot sign in by itself — and it needs no
// privilege: the machine holds the login, and its enrolment says whose it is.
// The login is sealed before it touches disk.
//
// A login the panel already holds (same address, else same name) is refreshed
// in place, which is how a broken one is mended; whoever first added it stays
// the one who can take it back. The contributor is given gateway access to
// their own login, so from now on their machine runs it from here — the copy
// left on the machine dies the first time the gateway refreshes it.
func (s *Server) handleContribute(w http.ResponseWriter, r *http.Request, dev Device) {
	var in struct {
		Name       string `json:"name"`
		Email      string `json:"email"`
		Plan       string `json:"plan"`
		Credential string `json:"credential"` // base64 of the credentials JSON
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "That request could not be read.")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(in.Credential)
	if err != nil || len(raw) == 0 {
		fail(w, 400, "That does not look like a stored login.")
		return
	}
	sealed, err := s.secret.Seal(raw)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = strings.TrimSpace(in.Email)
	}
	if name == "" {
		fail(w, 400, "Give the account a name.")
		return
	}
	// The contributor's own key is minted once, before the write, so a CAS
	// retry does not rotate it; it reaches their machine on its next check-in
	// like any other share.
	key, hash, err := NewToken()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	sealedKey, err := s.secret.Seal([]byte(key))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}

	var out struct {
		AccountID string `json:"accountId"`
		Name      string `json:"name"`
		Refreshed bool   `json:"refreshed"`
	}
	err = s.store.Mutate(func(d *Data) error {
		now := s.now()
		who := d.personName(dev.PersonID)
		a := d.accountByLogin(in.Email, name)
		if a == nil {
			d.Accounts = append(d.Accounts, Account{
				ID: newID(), Name: name, Email: strings.TrimSpace(in.Email), Plan: strings.TrimSpace(in.Plan),
				CreatedAt: now, Credential: sealed, AddedBy: dev.PersonID,
			})
			a = &d.Accounts[len(d.Accounts)-1]
			d.Log(now, who, "added "+a.Name+" from "+dev.Name)
		} else {
			a.Credential = sealed
			if e := strings.TrimSpace(in.Email); e != "" {
				a.Email = e
			}
			if p := strings.TrimSpace(in.Plan); p != "" {
				a.Plan = p
			}
			if a.AddedBy == "" {
				a.AddedBy = dev.PersonID
			}
			out.Refreshed = true
			d.Log(now, who, "refreshed the login for "+a.Name+" from "+dev.Name)
		}
		out.AccountID, out.Name = a.ID, a.Name
		// Access to their own login. Kept if they already have it, so re-adding
		// does not rotate a key their sessions are using.
		if _, has := d.shareFor(a.ID, dev.PersonID); !has {
			d.putShare(Share{ID: newID(), AccountID: a.ID, PersonID: dev.PersonID, KeyHash: hash, SealedKey: sealedKey, CreatedAt: now, GrantedBy: who})
		}
		return nil
	})
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	code := 201
	if out.Refreshed {
		code = 200
	}
	writeJSON(w, code, out)
}

// handleWithdraw is a member taking back the login their machine handed up.
// Only the person who added an account may do this (the admin removes any
// account from the site); everyone sharing it loses access, exactly as when
// the admin removes it.
func (s *Server) handleWithdraw(w http.ResponseWriter, r *http.Request, dev Device) {
	id := r.PathValue("id")
	var problem int
	err := s.store.Mutate(func(d *Data) error {
		a, ok := d.Account(id)
		if !ok {
			problem = 404
			return errors.New("That account is not on the panel any more.")
		}
		switch {
		case a.AddedBy == "":
			problem = 403
			return fmt.Errorf("%s was added before the panel recorded who adds what, so only its admin can remove it.", a.Name)
		case a.AddedBy != dev.PersonID:
			problem = 403
			return fmt.Errorf("%s was added by %s — only they, or the panel's admin, can take it back.", a.Name, d.personName(a.AddedBy))
		}
		removeAccount(d, id)
		d.Log(s.now(), d.personName(dev.PersonID), "took "+a.Name+" back off the panel")
		return nil
	})
	if err != nil {
		if problem == 0 {
			problem = 500
		}
		fail(w, problem, err.Error())
		return
	}
	w.WriteHeader(204)
}

// ---------------------------------------------------------------- people

func (s *Server) handleAddPerson(w http.ResponseWriter, r *http.Request) {
	who := s.actor(r) // the signed-in name every change is recorded under
	var in struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		fail(w, 400, "Give the person a name.")
		return
	}
	err := s.store.Mutate(func(d *Data) error {
		d.People = append(d.People, Person{ID: newID(), Name: in.Name, Email: in.Email, CreatedAt: s.now()})
		d.Log(s.now(), who, "added "+in.Name)
		return nil
	})
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]bool{"ok": true})
}

func (s *Server) handleRemovePerson(w http.ResponseWriter, r *http.Request) {
	who := s.actor(r) // the signed-in name every change is recorded under
	id := r.PathValue("id")
	err := s.store.Mutate(func(d *Data) error {
		p, ok := d.Person(id)
		if !ok {
			return errors.New("There is no such person.")
		}
		name := p.Name
		// Their shares go too, so their access ends everywhere at once.
		shares := d.Shares[:0]
		for _, sh := range d.Shares {
			if sh.PersonID != id {
				shares = append(shares, sh)
			}
		}
		d.Shares = shares
		devices := d.Devices[:0]
		for _, dev := range d.Devices {
			if dev.PersonID != id {
				devices = append(devices, dev)
			}
		}
		d.Devices = devices
		people := d.People[:0]
		for _, per := range d.People {
			if per.ID != id {
				people = append(people, per)
			}
		}
		d.People = people
		d.Log(s.now(), who, "removed "+name+", and their access ended")
		return nil
	})
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	w.WriteHeader(204)
}

// handleInvite mints an invite link for a person: a single-use, one-hour link
// that carries the join code, so setting someone up is "send them this link"
// rather than "read this code down the phone". The link lands on the panel's own
// /i/<code> page, which tells them what to do with it.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	who := s.actor(r) // the signed-in name every change is recorded under
	id := r.PathValue("id")
	code, hash, err := NewInviteCode()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	expires := s.now().Add(inviteLife)
	var invitee string
	err = s.store.Mutate(func(d *Data) error {
		p, ok := d.Person(id)
		if !ok {
			return errors.New("There is no such person.")
		}
		invitee = p.Name
		d.JoinCodes = append(d.JoinCodes, JoinCode{CodeHash: hash, PersonID: id, ExpiresAt: expires, InvitedBy: who})
		d.Log(s.now(), who, "made an invite link for "+p.Name)
		return nil
	})
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"url":       s.baseURL(r) + "/i/" + code,
		"code":      code,
		"person":    invitee,
		"expiresAt": expires,
	})
}

// handleInvitePage is the public page an invite link opens. It never reveals
// anything about the panel — only whether this particular code is still good and
// what to do with it — so it is safe to serve without a session.
func (s *Server) handleInvitePage(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	inv := invitePage{Link: s.baseURL(r) + "/i/" + code, State: "unknown"}
	d, err := s.store.Load()
	if err == nil {
		want := HashToken(strings.TrimSpace(code))
		for _, c := range d.JoinCodes {
			if !SameToken(want, c.CodeHash) {
				continue
			}
			inv.Invitee, inv.InvitedBy = d.personName(c.PersonID), c.InvitedBy
			switch {
			case !c.UsedAt.IsZero():
				inv.State = "used"
			case s.now().After(c.ExpiresAt):
				inv.State = "expired"
			default:
				inv.State = "good"
			}
			break
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(invitePageHTML(inv)))
}

// baseURL is the panel's address as a visitor reaches it: the configured
// canonical URL when it is set (behind a proxy the request's own host is the
// internal one), otherwise derived from the request.
func (s *Server) baseURL(r *http.Request) string {
	if u := strings.TrimRight(config.Env("PANEL_URL"), "/"); u != "" {
		return u
	}
	scheme := "https"
	if r.TLS == nil && !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func (s *Server) handleRemoveDevice(w http.ResponseWriter, r *http.Request) {
	who := s.actor(r) // the signed-in name every change is recorded under
	id := r.PathValue("id")
	err := s.store.Mutate(func(d *Data) error {
		dev, ok := d.Device(id)
		if !ok {
			return errors.New("There is no such machine.")
		}
		name, person := dev.Name, d.personName(dev.PersonID)
		out := d.Devices[:0]
		for _, x := range d.Devices {
			if x.ID != id {
				out = append(out, x)
			}
		}
		d.Devices = out
		d.Log(s.now(), who, fmt.Sprintf("cut off %s, %s's machine", name, person))
		return nil
	})
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	w.WriteHeader(204)
}

// ---------------------------------------------------------------- lending

// handleShare gives a person gateway access to an account and returns the
// gateway key to hand to their machine (shown once). Many people can be shared
// one account — that is the gateway model.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PersonID string  `json:"personId"`
		Hours    float64 `json:"hours"` // 0: until revoked
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "That request could not be read.")
		return
	}
	if in.Hours < 0 || in.Hours > 24*90 {
		fail(w, http.StatusBadRequest, "Access can be given for up to 90 days at a time, or until you take it back.")
		return
	}
	id := r.PathValue("id")
	if d, err := s.store.Load(); err == nil {
		if a, ok := d.Account(id); ok && !a.HasLogin() {
			fail(w, http.StatusBadRequest, a.Name+" has no login yet — authenticate it before sharing.")
			return
		}
	}
	key, err := s.store.IssueShareFor(id, in.PersonID, s.actor(r), time.Duration(in.Hours*float64(time.Hour)), func(k string) []byte { sealed, _ := s.secret.Seal([]byte(k)); return sealed })
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "gateway": gatewayURL()})
}

func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RevokeShare(r.PathValue("id"), s.actor(r)); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// gatewayURL is where members route their Claude Code, set on the panel's env.
func gatewayURL() string { return strings.TrimRight(config.Env("GATEWAY_URL"), "/") }

// ---------------------------------------------------------------- machines

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code    string `json:"code"`
		Machine string `json:"machine"`
		Version string `json:"version"` // the clawdh build joining, shown per machine
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "That request could not be read.")
		return
	}
	token, hash, err := NewToken()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	machine := strings.TrimSpace(in.Machine)
	if machine == "" {
		machine = "a machine"
	}
	var deviceID, personName string
	err = s.store.Mutate(func(d *Data) error {
		now := s.now()
		want := HashToken(strings.TrimSpace(in.Code))
		for i := range d.JoinCodes {
			c := &d.JoinCodes[i]
			if !SameToken(want, c.CodeHash) {
				continue
			}
			if !c.UsedAt.IsZero() {
				return errors.New("That code has already been used.")
			}
			if now.After(c.ExpiresAt) {
				return errors.New("That code has expired. Ask for a new one.")
			}
			c.UsedAt = now
			deviceID = newID()
			personName = d.personName(c.PersonID)
			d.Devices = append(d.Devices, Device{
				ID: deviceID, PersonID: c.PersonID, Name: machine,
				TokenHash: hash, EnrolledAt: now, LastSeen: now,
				Version: strings.TrimSpace(in.Version),
			})
			d.Log(now, d.personName(c.PersonID), "set up "+machine)
			return nil
		}
		return errors.New("That code is not one of ours.")
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"deviceId": deviceID, "token": token, "personName": personName})
}

// clientShare is one account a person may use through the gateway: its name,
// where to route (the gateway URL), and this person's key. The client turns each
// into an alias that runs Claude Code in gateway mode. Email is the login's
// address, so a machine that still has a local account for the same login can
// point at the share when that local copy stops working (the gateway's refresh
// rotates the single-use refresh token out from under it).
type clientShare struct {
	Account   string    `json:"account"`
	AccountID string    `json:"accountId"`
	Email     string    `json:"email,omitempty"`
	Slug      string    `json:"slug"`
	Gateway   string    `json:"gateway"`
	Key       string    `json:"key"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"` // when this access ends on its own; zero: until revoked
	// Contributed is whether this person's own machine handed the login up —
	// then their page offers to take it back, which nobody else's does.
	Contributed bool `json:"contributed,omitempty"`
}

// handleCheckin is the whole of what a machine asks: what may I use?
//
// The answer is the complete list of accounts shared with this person through
// the gateway, each with its gateway URL and this person's key — never the
// Claude login itself, which stays on the server. It is a complete list, not a
// diff, so a key the machine holds and is not told about here is one it must
// stop using; that is how taking access away works without the panel reaching
// the machine. The machine asks every half minute and acts on the answer.
//
// It is deliberately flat: no notion of who decided or why, so a client that
// does not model the panel cannot get the panel's rules wrong.
func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request, dev Device) {
	// The machine tells us, every check-in, whether its owner has remote help on.
	// It is the consent the jobs channel runs on: a machine reporting false is
	// never offered for a job and is served nothing to run. It also says which
	// clawdh build it runs, so the People tab can show a machine that is behind.
	var in struct {
		Remote  bool   `json:"remote"`
		Version string `json:"version"`
	}
	_ = readJSON(r, &in)

	if err := s.store.Mutate(func(d *Data) error {
		now := s.now()
		for i := range d.Devices {
			if d.Devices[i].ID == dev.ID {
				d.Devices[i].LastSeen = now
				d.Devices[i].Remote = in.Remote
				if v := strings.TrimSpace(in.Version); v != "" {
					d.Devices[i].Version = v
				}
			}
		}
		return nil
	}); err != nil {
		fail(w, 500, err.Error())
		return
	}

	d, _ := s.store.Load()

	var shares []clientShare
	if gw := gatewayURL(); gw != "" {
		slugs := shareSlugs(d, dev.PersonID)
		for _, sh := range d.Shares {
			if sh.PersonID != dev.PersonID || !sh.Live(s.now()) {
				continue
			}
			acct, ok := d.Account(sh.AccountID)
			if !ok || len(sh.SealedKey) == 0 {
				continue
			}
			plain, err := s.secret.Open(sh.SealedKey)
			if err != nil {
				continue
			}
			shares = append(shares, clientShare{
				Account: acct.Name, AccountID: acct.ID, Email: acct.Email, Slug: slugs[acct.ID],
				Gateway: gw, Key: string(plain), ExpiresAt: sh.ExpiresAt,
				Contributed: acct.AddedBy != "" && acct.AddedBy == dev.PersonID,
			})
		}
	}

	// Any consented remote jobs waiting for this machine ride back on the same
	// reply — but only if its owner has remote help on, so a machine that never
	// opted in is never handed anything to run.
	var jobs []Job
	if s.jobs != nil && in.Remote {
		jobs, _ = s.jobs.PendingJobs(r.Context(), dev.ID)
	}
	// Short notices for the person at this machine (quota, a broken login) ride
	// back too, so the machine can surface them without polling anything else.
	notices := s.checkinNotices(r.Context(), dev.PersonID, d)
	// The gateway's real 5h/weekly utilisation for each account this person can
	// use, so their machine shows usage for a shared login without probing it.
	windows := s.checkinWindows(r.Context(), dev.PersonID, d)
	writeJSON(w, 200, map[string]any{"gateway": shares, "jobs": jobs, "notices": notices, "windows": windows})
}

// slugify makes a shell-safe short name for an account's alias.
// shareSlug is the short name a person types for a shared account: `clawdh
// shared <slug>`. A login is usually named by its email, and nobody wants to
// type "ehtishamdevhouseco" — so an email-shaped name shortens to the part
// before the @. Anything else slugifies whole.
func shareSlug(name string) string {
	if at := strings.IndexByte(name, '@'); at > 0 {
		if s := slugify(name[:at]); s != "account" {
			return s
		}
	}
	return slugify(name)
}

// shareSlugs gives every account shared with a person a slug unique among
// their shares, stable across check-ins: accounts are visited in id order and
// a repeat gets a -2, -3 suffix. Both the check-in's share list and its usage
// windows are named through this, so the two always agree.
func shareSlugs(d Data, personID string) map[string]string {
	var ids []string
	for _, sh := range d.Shares {
		if sh.PersonID == personID {
			ids = append(ids, sh.AccountID)
		}
	}
	sort.Strings(ids)
	out := make(map[string]string, len(ids))
	taken := make(map[string]bool, len(ids))
	for _, id := range ids {
		acct, ok := d.Account(id)
		if !ok {
			continue
		}
		base := shareSlug(acct.Name)
		slug := base
		for n := 2; taken[slug]; n++ {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		taken[slug] = true
		out[id] = slug
	}
	return out
}

func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	if out := strings.Trim(b.String(), "-"); out != "" {
		return out
	}
	return "account"
}
