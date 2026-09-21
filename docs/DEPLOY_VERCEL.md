# Hosting the admin panel on Vercel

The panel is one Go serverless function (`api/index.go`) that serves the admin
UI and the API every machine's client talks to. Its state lives in Postgres, not
on disk, because a serverless panel is many short-lived instances at once and the
one-machine-per-account rule is held by a database compare-and-swap they share.
The client on each machine is the ordinary clawdh binary: it checks in every thirty
seconds, obeys the panel, and self-updates from the GitHub `latest` release.

## What is already set up

The project is linked and deployed:

- Project: `clawdh` on Vercel, live at **https://clawdh.vercel.app**
- `CLAWDH_PANEL_KEY` (the key that seals stored logins) is set in the Production
  environment. Keep your copy — losing it makes every stored login unreadable.
- Vercel's own authentication wall (SSO) is turned off for this project, so the
  panel's own password is the gate rather than a second Vercel login the client
  machines could not pass.

Until the two steps below are done, the panel answers every request with
"set DATABASE_URL" — the function is running, it just has nowhere to keep state.

## The two remaining steps (dashboard, ~3 min)

Both are on your Vercel account, which is why they are yours to click.

1. **Give it a database.** Vercel dashboard → the `clawdh` project →
   **Storage** → **Create Database** → **Neon** (Postgres, has a free tier).
   Accept the defaults and attach it to the project. Vercel injects `DATABASE_URL`
   into the environment automatically; the panel creates its own table on first
   request. Redeploy once (Deployments → ⋯ → Redeploy) so the new variable is
   picked up.

2. **Turn on auto-deploy** so the server updates itself on every release.
   Project → **Settings** → **Git** → connect it to `Saif0089/clawdh` (this asks
   you to link your GitHub account to Vercel once). From then on every push to
   `main` redeploys the panel — in lockstep with the client release the CI
   pipeline publishes from the same push.

Then open https://clawdh.vercel.app and set the admin password.

## Enrolling machines against it

On the panel, People → **Invite someone** makes one link. Opened on a computer
that has clawdh, it joins that computer in a click; on one without, the same
page gives a one-line install that joins as it installs. The equivalent on the
command line:

```sh
clawdh join <invite-link>
```

A login signed in on a joined machine is handed to the panel from that machine
— **Add to panel** on the machine's clawdh page, or:

```sh
clawdh panel push <account>
```

The machine's membership is the credential for both; the panel's password is
only ever typed into the panel itself.

## What this defends against, and what it does not

The panel keeps real logins in a Postgres row, sealed with `CLAWDH_PANEL_KEY`. A
copy of the database alone is not a working set of logins; the key is needed too,
and it lives only in Vercel's environment and wherever you kept it. It does not
defend against someone who already controls the Vercel project or the database. A
machine that cannot reach the panel keeps what it was last told it had, so taking
an account back reaches an online machine within thirty seconds and a sleeping one
when it wakes. If a login may have been copied while someone held it, sign that
account out at Anthropic after taking it back — that is what makes an old copy
useless.

## Alternative to step 2: deploy from CI instead of Git integration

If you would rather not connect GitHub to Vercel, the pipeline has a dormant
`deploy-panel` job. Set repo secrets `VERCEL_TOKEN` (mint one at Vercel → Account
Settings → Tokens), `VERCEL_ORG_ID` = `team_6DQvdeYVNBbVbDQ1ZVsez1rb`, and
`VERCEL_PROJECT_ID` = `prj_XCexE8s0MWmP5aYZg4AiGcLKZr15`, plus the repo variable
`VERCEL_DEPLOY` = `true`. Use one mechanism or the other, not both.
