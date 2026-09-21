package panel

import (
	"fmt"
	"html"
	"net/url"
	"strings"

	"clawdh/internal/config"
)

// Install one-liners, shown on the invite page so a new person has real steps
// rather than "ask your admin". Kept here (not just the README) because the
// invite page is the first thing someone who has never seen clawdh will read.
const (
	installRepo   = "https://github.com/Saif0089/clawdh"
	claudeCodeURL = "https://claude.com/claude-code"
	installUnix   = "https://raw.githubusercontent.com/Saif0089/clawdh/main/install.sh"
	installWin    = "https://raw.githubusercontent.com/Saif0089/clawdh/main/install.ps1"
)

// localPage is where clawdh's own page answers on the computer the invite is
// opened on. The invite's one button opens it with the invite in hand, and the
// page joins on its own — so a person with clawdh already installed never
// copies anything.
var localPage = fmt.Sprintf("http://127.0.0.1:%d/", config.DefaultPort)

// invitePage is what the page an invite link opens is built from: the link
// itself, whether it is still good, and who is inviting whom.
type invitePage struct {
	Link      string
	State     string // good | used | expired | unknown
	Invitee   string // the person the link joins the computer as
	InvitedBy string // the signer who made it
}

// joinCommands are the one-line installs that also join, per platform: the
// installer takes the invite and runs `clawdh join` once clawdh is in place,
// so "install" and "join" are one paste for someone new to this.
func joinCommands(link string) (unix, win string) {
	return fmt.Sprintf(`curl -fsSL %s | sh -s -- --join %q`, installUnix, link),
		fmt.Sprintf(`$env:CLAWDH_JOIN = "%s"; irm %s | iex`, link, installWin)
}

// invitePageHTML is the page an invite link opens in a browser. It is a single
// self-contained document: an invite is often opened by someone who has never
// seen clawdh, so it explains itself and stands on its own. It shares the
// panel's colour tokens and type so the whole product looks like one thing.
//
// It has one thing to do per situation and never asks the person to handle
// the link again: clawdh already on this computer — one button, which opens
// clawdh's page with the invite and joins; not yet — one command, which
// installs clawdh and joins.
func invitePageHTML(inv invitePage) string {
	var body string
	switch inv.State {
	case "expired", "used":
		reason := "This invite has expired."
		if inv.State == "used" {
			reason = "This invite has already been used."
		}
		body = `
      <h1>Link no longer works</h1>
      <p class="lead">` + reason + ` Ask whoever sent it for a fresh one — invites last about an hour and work once.</p>`
	default:
		title := "You've been invited to Claude"
		if inv.InvitedBy != "" {
			title = html.EscapeString(inv.InvitedBy) + " invited you to Claude"
		}
		as := ""
		if inv.Invitee != "" {
			as = " as <b>" + html.EscapeString(inv.Invitee) + "</b>"
		}
		note := ""
		if inv.State == "unknown" {
			note = `<p class="muted small">We couldn't confirm this link here. If it's from a different panel, the steps still apply.</p>`
		}
		unixCmd, winCmd := joinCommands(inv.Link)
		joinURL := localPage + "?invite=" + url.QueryEscape(inv.Link)
		body = `
      <h1>` + title + `</h1>
      <p class="lead">You'll run Claude Code on an account shared with you` + as + ` — nothing to sign in to, nothing to keep. Works once, for about an hour.</p>

      <div class="way">
        <b>Have clawdh on this computer?</b>
        <a class="button primary" href="` + html.EscapeString(joinURL) + `">Join on this computer</a>
        <span class="det">Opens your clawdh page and joins. Nothing happened? clawdh isn't installed here yet — use the command below.</span>
      </div>

      <div class="way">
        <b>New to clawdh? One command installs it and joins.</b>
        <div class="os-tabs" role="tablist">
          <button type="button" class="os-tab is-on" data-os="unix">macOS / Linux</button>
          <button type="button" class="os-tab" data-os="win">Windows</button>
        </div>
        <div class="copybox">
          <code id="install-cmd">` + html.EscapeString(unixCmd) + `</code>
          <button type="button" class="copy" data-copy-target="install-cmd">Copy</button>
        </div>
        <span class="det small">Paste it into a terminal. Needs the <a href="` + claudeCodeURL + `" target="_blank" rel="noopener">Claude Code CLI</a> ·
          <a href="` + installRepo + `" target="_blank" rel="noopener">about clawdh</a></span>
      </div>
      ` + note
		// The OS tabs swap the command; the page's script needs both.
		body += `<script>window.__joinCmds = { unix: ` + jsString(unixCmd) + `, win: ` + jsString(winCmd) + ` };</script>`
	}

	return `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>clawdh invite</title>
<style>` + unifiedTokens + `
  body { display:flex; min-height:100vh; align-items:center; justify-content:center; padding:28px 20px; }
  .card { width:100%; max-width:560px; background:var(--raised); border:1px solid var(--line);
          border-radius:20px; padding:44px 44px 36px; box-shadow:0 24px 60px -24px rgba(0,0,0,.7);
          animation:rise .5s cubic-bezier(.2,.7,.3,1) both; }
  @keyframes rise { from { opacity:0; transform:translateY(14px); } to { opacity:1; transform:none; } }
  @media (prefers-reduced-motion: reduce) { .card { animation:none; } }

  h1 { margin:0 0 12px; font-size:30px; letter-spacing:-.025em; line-height:1.12; }
  .lead { margin:0 0 26px; color:var(--muted); font-size:17px; line-height:1.5; }
  .lead b { color:var(--ink); }

  .way { padding:22px 0 0; margin-top:22px; border-top:1px solid var(--line); }
  .way:first-of-type { border-top:none; margin-top:0; padding-top:0; }
  .way > b { display:block; font-size:17px; margin-bottom:12px; }
  .det { display:block; color:var(--muted); font-size:14.5px; line-height:1.5; margin-top:12px; }
  .det.small { font-size:13px; margin-top:10px; }
  .det a { color:var(--primary); text-decoration:none; }
  .det a:hover { text-decoration:underline; }

  .button { display:inline-block; padding:13px 22px; border-radius:12px; font-weight:700; font-size:16px;
    text-decoration:none; transition:transform .1s ease, filter .15s ease; }
  .button.primary { background:var(--primary); color:var(--primary-ink); box-shadow:0 8px 24px -10px var(--primary); }
  .button.primary:hover { filter:brightness(1.08); transform:translateY(-1px); }

  .os-tabs { display:inline-flex; gap:4px; margin:0 0 8px; padding:3px; background:var(--sunken);
    border:1px solid var(--line); border-radius:9px; }
  .os-tab { background:none; border:none; color:var(--muted); font:inherit; font-size:13px; font-weight:500;
    padding:6px 12px; border-radius:6px; cursor:pointer; transition:all .15s ease; }
  .os-tab:hover { color:var(--ink); }
  .os-tab.is-on { background:var(--raised-2); color:var(--ink); }

  .copybox { display:flex; gap:8px; margin-top:2px; }
  .copybox code { flex:1; min-width:0; overflow-x:auto; white-space:nowrap; background:var(--sunken);
    border:1px solid var(--line); border-radius:10px; padding:13px 14px;
    font-family:var(--mono); font-size:13.5px; color:var(--ink); scrollbar-width:thin; }
  .copybox .copy { flex:none; background:var(--raised-2); color:var(--ink); border:1px solid var(--line);
    border-radius:10px; padding:0 18px; font:inherit; font-size:14px; font-weight:600; cursor:pointer;
    transition:transform .1s ease, filter .15s ease, background .15s ease; }
  .copybox .copy:hover { background:#252c37; transform:translateY(-1px); }
  .copybox .copy:active { transform:translateY(0); }
  .copybox .copy.copied { background:var(--ok); color:var(--primary-ink); border-color:var(--ok); }

  .muted { color:var(--muted); } .small { font-size:13px; margin:16px 0 0; }
  @media (max-width:560px){ .card { padding:32px 24px 28px; } h1 { font-size:25px; } }
</style></head>
<body>
  <div class="card">` + body + `</div>
  <script>
    // One copy handler for every copybox button.
    document.querySelectorAll('.copy[data-copy-target]').forEach(function(btn){
      btn.addEventListener('click', function(){
        var el = document.getElementById(btn.getAttribute('data-copy-target'));
        if(!el) return;
        navigator.clipboard.writeText(el.textContent).then(function(){
          var t = btn.textContent; btn.textContent = 'Copied'; btn.classList.add('copied');
          setTimeout(function(){ btn.textContent = t; btn.classList.remove('copied'); }, 1500);
        }).catch(function(){ btn.textContent = 'Copy failed'; });
      });
    });
    // OS tabs swap the install command; default to the visitor's platform.
    (function(){
      var cmd = document.getElementById('install-cmd');
      var cmds = window.__joinCmds;
      if(!cmd || !cmds) return;
      var tabs = document.querySelectorAll('.os-tab');
      function pick(os){
        cmd.textContent = cmds[os] || cmds.unix;
        tabs.forEach(function(t){ t.classList.toggle('is-on', t.getAttribute('data-os')===os); });
      }
      tabs.forEach(function(t){ t.addEventListener('click', function(){ pick(t.getAttribute('data-os')); }); });
      var ua = (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || navigator.userAgent || '';
      if(/win/i.test(ua)) pick('win');
    })();
  </script>
</body></html>`
}

// jsString renders a Go string as a safe JavaScript string literal.
func jsString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`, "<", `\x3c`, ">", `\x3e`)
	return "'" + r.Replace(s) + "'"
}

// unifiedTokens is the one design system the panel, the local client page and
// this invite page are built from: the same palette, type and radius, so the
// three never look like separate products. It is CSS custom properties plus the
// couple of base rules every page needs, and nothing page-specific.
var unifiedTokens = strings.TrimSpace(`
  :root {
    color-scheme: dark;
    --ground:#0F1216; --raised:#171B21; --raised-2:#1E242D; --sunken:#0B0E12; --line:#262C34;
    --ink:#E7EBF0; --muted:#9AA4B2; --faint:#626D7C;
    --primary:#6E8BFF; --primary-ink:#0B0E12;
    --ok:#46C08A; --warn:#E0A83E; --crit:#E05C53;
    --radius:12px;
    --font: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Ubuntu, Helvetica, Arial, sans-serif;
    --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
  }
  * { box-sizing:border-box; }
  body { margin:0; background:var(--ground); color:var(--ink);
         font-family:var(--font); font-size:16px; line-height:1.5;
         -webkit-font-smoothing:antialiased; }
`)
