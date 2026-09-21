package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clawdh/internal/accounts"
	"clawdh/internal/config"
	"clawdh/internal/remotejobs"
	"clawdh/internal/shellrc"
	"clawdh/panel"
)

// defaultPanelAddr is the panel's own port, one above the local UI's. It binds
// loopback by default: a panel is only useful to other people once it is
// deliberately exposed, and defaulting to that would put every login it holds
// on the network the moment someone tried it out.
const defaultPanelAddr = "127.0.0.1:47933"

func cmdPanel(args []string) int {
	if len(args) == 0 {
		return panelStatusCmd()
	}
	switch args[0] {
	case "status":
		return panelStatusCmd()
	case "serve":
		return panelServe(args[1:])
	case "check":
		return panelCheck(args[1:])
	case "push":
		return panelPush(args[1:])
	case "genkey":
		return panelGenkey()
	case "help", "--help", "-h":
		panelUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "clawdh panel: unknown command %q\n\n", args[0])
		panelUsage(os.Stderr)
		return 1
	}
}

func panelUsage(w *os.File) {
	fmt.Fprint(w, `clawdh panel — share Claude logins with other people, through a gateway

  clawdh panel serve [--addr host:port]   run the panel (default `+defaultPanelAddr+`)
  clawdh panel check                      ask the panel what is shared with this machine, now
  clawdh panel push <account>             add an account's login to the panel so it can be shared
  clawdh panel genkey                     print a new sealing key for a hosted panel

Connecting a machine is `+"`clawdh join <invite-link>`"+`. Everything a machine does
with its panel — hearing what it may use, adding a login, taking one back — it
does as the person it joined as; no password is ever typed here. The password
is for the panel's own site. All of this also lives on the clawdh web page, so
a person who does not use the terminal never has to.

Serving on 127.0.0.1 keeps the panel to this machine. To let other people
reach it, give --addr an address they can see, and put it behind TLS.
`)
}

func panelPaths() (store, key, client string, err error) {
	base, err := config.HomeDir()
	if err != nil {
		return "", "", "", err
	}
	client, err = config.PanelClientFile()
	if err != nil {
		return "", "", "", err
	}
	return filepath.Join(base, "panel.json"),
		filepath.Join(base, "panel.key"),
		client, nil
}

func panelServe(args []string) int {
	addr := defaultPanelAddr
	for i := 0; i < len(args); i++ {
		if args[i] == "--addr" && i+1 < len(args) {
			addr = args[i+1]
			i++
		}
	}
	storePath, keyPath, _, err := panelPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	secret, err := panel.LoadSecret(keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	srv := panel.NewServer(panel.NewStore(storePath), secret, nil, nil) // local file panel: no metering DB, no remote jobs

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clawdh: cannot listen on %s: %v\n", addr, err)
		return 1
	}
	fmt.Printf("clawdh panel on http://%s\n", ln.Addr())
	if !strings.HasPrefix(addr, "127.0.0.1") && !strings.HasPrefix(addr, "localhost") {
		fmt.Println("This panel is reachable from the network. Put it behind TLS before anyone signs in over it.")
	}
	if err := http.Serve(ln, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	return 0
}

// cmdJoin connects this machine to a panel from the one thing a person is
// sent: the invite link. `clawdh join https://panel/i/<code>` pulls the panel
// address and the code out of it.
func cmdJoin(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: clawdh join <invite-link>")
		return 1
	}
	server, code, ok := parseInvite(args[0])
	if !ok {
		fmt.Fprintln(os.Stderr, "clawdh: that does not look like an invite link. Paste the whole link you were sent.")
		return 1
	}
	return enrollMachine(server, code)
}

// parseInvite pulls the panel URL and one-shot code out of an invite link. It
// accepts the code in the path (/i/<code>, /join/<code>), the fragment
// (#<code>), or a ?code= query, so a link that survived being pasted through a
// chat app in any of those shapes still works.
func parseInvite(link string) (server, code string, ok bool) {
	u, err := neturl.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" {
		return "", "", false
	}
	origin := u.Scheme + "://" + u.Host
	if q := u.Query().Get("code"); q != "" {
		return origin, q, true
	}
	if u.Fragment != "" {
		return origin, u.Fragment, true
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 2 && (parts[len(parts)-2] == "i" || parts[len(parts)-2] == "join") {
		return origin, parts[len(parts)-1], true
	}
	return "", "", false
}

// enrollMachine trades a code for this machine's token and remembers the panel.
func enrollMachine(server, code string) int {
	_, _, clientPath, err := panelPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	name, _ := os.Hostname()
	if name == "" {
		name = "a machine"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := panel.Enroll(ctx, server, code, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	if err := panel.SaveClientConfig(clientPath, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	who := cfg.PersonName
	if who == "" {
		who = name
	}
	fmt.Printf("This machine is connected to %s. You are %s here.\n", server, who)
	return panelCheck(nil)
}

// panelClient builds the check-in client over this machine's real accounts.
func panelClient() (*panel.Client, error) {
	_, _, clientPath, err := panelPaths()
	if err != nil {
		return nil, err
	}
	cfg, err := panel.LoadClientConfig(clientPath)
	if err != nil {
		return nil, err
	}
	accountsFile, err := config.AccountsFile()
	if err != nil {
		return nil, err
	}
	accountsDir, err := config.AccountsDir()
	if err != nil {
		return nil, err
	}
	sharesPath, err := config.SharesFile()
	if err != nil {
		return nil, err
	}
	mgr := accounts.NewManager(accounts.NewStore(accountsFile), accountsDir)
	return &panel.Client{
		Config:     cfg,
		Accounts:   mgr,
		SharesPath: sharesPath,
		AfterChange: func() {
			// Nothing per-account is written to shell rc files any more — every
			// account runs as `clawdh <name>` / `clawdh shared <name>` — but the
			// managed block is re-synced so aliases from earlier versions go.
			home, err := os.UserHomeDir()
			if err != nil {
				return
			}
			_ = shellrc.NewSyncer(home).Sync(nil)
		},
	}, nil
}

// gatewaySharesFor reads the cached gateway shares, or none on any error — a
// missing or unreadable cache just means no shared accounts.
func gatewaySharesFor(path string) []panel.GatewayShare {
	shares, _ := panel.LoadShares(path)
	return shares
}

// slugifyName mirrors the panel's slug rule for display, so the alias printed
// here matches the one the shell actually has.
func slugifyName(name string) string {
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

func panelCheck(_ []string) int {
	c, err := panelClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	if !c.Config.Configured() {
		fmt.Fprintln(os.Stderr, "clawdh: this machine has not joined a panel. Open your invite link, or run `clawdh join <invite-link>`.")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	change, err := c.CheckIn(ctx)
	for _, name := range change.Gained {
		fmt.Printf("You can now use %s — run it with `clawdh shared %s`.\n", name, slugifyName(name))
	}
	for _, name := range change.Lost {
		fmt.Printf("%s is no longer shared with you.\n", name)
	}
	if errors.Is(err, panel.ErrNotEnrolled) {
		fmt.Fprintln(os.Stderr, "clawdh: this machine is no longer enrolled with the panel.")
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	if change.Empty() {
		fmt.Println("Nothing changed.")
	}
	return 0
}

// panelPush hands an account's login up to the panel this machine is joined
// to, so it can be shared through the gateway. The panel cannot sign an account
// in by itself, so this is how an account gets something to lend. It happens as
// the person this machine joined as — no password, no URL: the enrolment is the
// credential, and giving your own login away needs no privilege.
func panelPush(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: clawdh panel push <account>")
		return 1
	}
	name := args[0]

	list, err := loadAccounts()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	var acct accounts.Account
	for _, a := range list {
		if strings.EqualFold(a.Slug, name) || strings.EqualFold(a.Name, name) {
			acct = a
		}
	}
	if acct.ID == "" {
		fmt.Fprintf(os.Stderr, "clawdh: no account called %q on this machine.\n", name)
		return 1
	}
	raw, err := accounts.CaptureLogin(acct.ConfigDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clawdh: %s is not signed in on this machine, so there is no login to hand to the panel.\n", acct.Name)
		fmt.Fprintf(os.Stderr, "      Open http://127.0.0.1:%d, connect %s, finish the browser login, then run this again.\n", config.DefaultPort, acct.Slug)
		return 1
	}
	c, err := panelClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	if !c.Config.Configured() {
		fmt.Fprintln(os.Stderr, "clawdh: this machine is not connected to a panel, so there is nowhere to add the login.")
		fmt.Fprintln(os.Stderr, "      Ask the panel's admin for an invite link and run `clawdh join <invite-link>` first.")
		return 1
	}
	// The login's address goes with it, as the web page sends it: it is how a
	// machine finds the share that runs a login whose local copy has died (see
	// missingLogin), and how the panel knows a re-add is the same account.
	email, plan := "", ""
	for _, l := range accounts.DiscoverLogins(list) {
		if l.ConfigDir == acct.ConfigDir {
			email, plan = l.Email, l.Plan
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	added, err := c.Contribute(ctx, acct.Name, email, plan, raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	// Access to it comes back on a check-in; do one now rather than making the
	// person wait for the next tick.
	change, _ := c.CheckIn(ctx)
	if added.Refreshed {
		fmt.Printf("%s was already on the panel; its login is refreshed.\n", added.Name)
	} else {
		fmt.Printf("%s is on the panel and ready to share, added as %s.\n", added.Name, c.Config.PersonName)
	}
	slug := slugifyName(added.Name)
	for _, sh := range gatewaySharesFor(c.SharesPath) {
		if sh.AccountID == added.AccountID {
			slug = sh.Slug
		}
	}
	_ = change
	fmt.Println()
	fmt.Printf("From now on run this login through the gateway — `clawdh shared %s` — not as `clawdh %s`.\n", slug, acct.Slug)
	fmt.Printf("The gateway refreshes the login itself, and a Claude login can only be refreshed from one place: the copy here stops\n")
	fmt.Printf("working the first time the gateway refreshes it, and `clawdh %s` will say so. Reconnect it on the clawdh page to have a separate local login again.\n", acct.Slug)
	fmt.Printf("To take it back off the panel: the clawdh page, under “Shared with you”.\n")
	return 0
}

// checkInEvery is how often an enrolled machine asks the panel what it holds.
// It is the worst case for how long someone keeps an account after it was
// taken back, so it is short; the request is tiny and answered from a file.
const checkInEvery = 30 * time.Second

// watchPanel keeps this machine in step with the panel it is enrolled with, for
// as long as clawdh is running. It is silent when nothing changes, which is
// almost always, and gives up quietly when this machine answers to no panel.
func watchPanel(ctx context.Context) {
	t := time.NewTicker(checkInEvery)
	defer t.Stop()
	for {
		// Reload the config each tick rather than once at startup, so a machine
		// joined from the web page (or by `clawdh join`) after the service
		// was already running is picked up without a restart.
		c, err := panelClient()
		if err != nil || !c.Config.Configured() {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				continue
			}
		}
		change, err := c.CheckIn(ctx)
		for _, name := range change.Gained {
			fmt.Printf("clawdh: %s is now shared with this machine — run it with `clawdh shared %s`.\n", name, slugifyName(name))
			notifyBrief("Now shared with you: " + name)
		}
		for _, name := range change.Lost {
			fmt.Printf("clawdh: %s is no longer shared with this machine.\n", name)
			notifyBrief("No longer shared with you: " + name)
		}
		// Short per-person notices (a quota warning, a broken login) the panel
		// worked out for this machine's owner, each shown at most once.
		showNotices(change.Notices)
		// The gateway's usage reading for each shared account, for the page.
		if err == nil {
			panel.SaveWindows(change.Windows)
		}
		// Any consented remote jobs the panel handed back run here, each announced
		// as it goes. Only ever non-empty when the owner turned remote help on.
		if len(change.Jobs) > 0 {
			runRemoteJobs(ctx, c, change.Jobs)
		}
		// A held request nobody answered within the hour is declined for them.
		remotejobs.Default.Expire(ctx)
		if errors.Is(err, panel.ErrNotEnrolled) {
			fmt.Fprintln(os.Stderr, "clawdh: this machine is no longer connected to the panel; its shared accounts have been removed.")
			return
		}
		// Any other failure is the panel being unreachable, which is not an
		// event: a machine that cannot ask keeps what it was last told it had.
		// Saying so every thirty seconds would fill the log with the fact that
		// a laptop is on a train.
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// panelGenkey prints a fresh sealing key for a panel that runs somewhere with no
// disk of its own — a serverless deployment. The value goes in that host's
// environment as CLAWDH_PANEL_KEY, and is the only thing that can open the logins
// the panel holds, so it is printed once and never kept by clawdh.
func panelGenkey() int {
	key, err := panel.GenerateKeyBase64()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clawdh:", err)
		return 1
	}
	fmt.Println(key)
	fmt.Fprintln(os.Stderr, "Set this as CLAWDH_PANEL_KEY in the panel's environment. Keep it — it cannot be recovered,")
	fmt.Fprintln(os.Stderr, "and losing it makes every stored login unreadable.")
	return 0
}

// panelStatusCmd prints, in plain terms, whether this machine is connected to a
// panel, which one, who it is there, and what it is holding — so the CLI says
// what is going on without the reader having to know the sub-commands.
func panelStatusCmd() int {
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

	if !cfg.Configured() {
		fmt.Println("This machine is not connected to a panel.")
		fmt.Println()
		fmt.Println("Connect it from the clawdh page:")
		fmt.Printf("    http://127.0.0.1:%d\n", config.DefaultPort)
		fmt.Println("or paste your invite link:")
		fmt.Println("    clawdh join <invite-link>")
		return 0
	}

	who := cfg.PersonName
	if who == "" {
		who = "this machine"
	}
	fmt.Printf("Connected to %s\n", cfg.Server)
	fmt.Printf("You are %s there.\n", who)

	// The accounts shared with this machine through the gateway.
	shares := sharedAccounts()
	fmt.Println()
	if len(shares) == 0 {
		fmt.Println("Nothing is shared with this machine yet.")
	} else {
		fmt.Println("Shared with you:")
		for _, sh := range shares {
			fmt.Printf("    %-24s clawdh shared %s\n", sh.Account, sh.Slug)
		}
	}
	return 0
}

// sharedAccounts is the gateway shares this machine currently has, from the
// cache the check-in keeps.
func sharedAccounts() []panel.GatewayShare {
	path, err := config.SharesFile()
	if err != nil {
		return nil
	}
	return gatewaySharesFor(path)
}
