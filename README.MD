# KindleVibe

KindleVibe is a small local Go server that shows GitHub Copilot and Codex usage in a Kindle-friendly web page. It is designed for e-ink screens: high contrast, portrait-friendly layout, large typography, and automatic refresh.

![Demo](demo.jpg)

## Features

- **Kindle optimized:** Minimal, high-contrast UI for e-ink browsers.
- **Portrait dashboard:** Single-screen layout tuned for a 1080 x 1440 Kindle display.
- **Auto-refresh:** Refreshes every 10 seconds with standard HTML meta refresh.
- **Current providers:** GitHub Copilot and Codex.
- **Exchange snapshot:** Also renders BTC/USD, USD/CNY, and AUD/CNY at the bottom of the screen.
- **Local-only:** Reads usage from local auth state and local session files.
- **Simple startup:** `make` rebuilds and starts the server.

## Prerequisites

- Go 1.21 or later.
- GitHub Copilot authenticated locally.
- Codex installed and used locally so session files exist.

## Getting Started

1. **Clone the repository**

   ```bash
   git clone https://github.com/lexrus/KindleVibe.git
   cd KindleVibe
   ```

2. **Configure the server**

   Edit `config.yaml` to set the port and enabled agents.
   Keep secrets out of this file. `config.yaml` is a sample config that is safe to commit.

3. **Build and run**

   ```bash
   make
   ```

   `make` rebuilds `main.go` and starts the server on the configured port.

4. **Open it on your Kindle**

   Visit:

   ```text
   http://<your-local-ip>:8080
   ```

## Configuration

`config.yaml`:

```yaml
server:
  port: 8080
agents:
  copilot:
    enabled: true
  codex:
    enabled: true
```

`config.yaml` only controls which providers are shown and which port the server listens on.
It does not store tokens. For local credentials, KindleVibe prefers auto-discovery from files you
already have, with an environment-variable override for Copilot.

## Data Sources

KindleVibe reads provider data when the `/` page is requested. For each enabled provider, `main.go` tries to build an `AgentStats` block and logs errors instead of failing the whole page, so one broken provider does not stop the dashboard from rendering.

### GitHub Copilot

KindleVibe fetches Copilot usage from GitHub's internal Copilot endpoint using local auth state.

Token resolution order:

1. `COPILOT_API_TOKEN`
2. `~/.config/github-copilot/apps.json`

#### Recommended setup

If you already use GitHub Copilot locally, you usually do not need to configure anything else.
KindleVibe will try to read the same local `apps.json` state that the Copilot client uses.

If you prefer an explicit override, set `COPILOT_API_TOKEN` in your shell before running `make`:

```bash
export COPILOT_API_TOKEN=your_token_here
make
```

To make it persistent for your shell, add the export line to your shell profile such as `~/.zshrc`,
reload the shell, and then start KindleVibe again.

Do not put `COPILOT_API_TOKEN` in `config.yaml`. That file is intended to remain safe to commit to GitHub.

If `apps.json` is used, `main.go` sorts the available app entries and uses the first non-empty `oauth_token` it finds.

After resolving a token, KindleVibe sends:

- `GET https://api.github.com/copilot_internal/user`
- `Authorization: token <token>`
- editor-style headers so GitHub returns the same quota payload the local Copilot client expects

It then parses:

- `copilot_plan` for the detail line
- `quota_snapshots` for usage bars
- `quota_reset_date_utc` or `quota_reset_date` for the reset timestamp

The usage bars are built from quota snapshots with flexible name matching:

- `Premium` uses the first matching snapshot among `premium_interactions`, `premium`, `completions`, or `code`
- `Chat` uses the `chat` snapshot

Used percentage is calculated in one of two ways:

- `100 - percent_remaining`, if `percent_remaining` exists
- `100 - (remaining / entitlement * 100)`, if only raw quota numbers exist

If no usable quota snapshots are returned, KindleVibe logs the issue and still keeps the page running.

### Codex

KindleVibe reads Codex rate-limit data from local session files:

- `~/.codex/sessions`
- `~/.codex/archived_sessions`

There is no Codex token field in `config.yaml`.
To show the Codex section, you only need:

- `agents.codex.enabled: true`
- a local Codex installation that has already been used at least once on this machine

In other words, users do not need to "configure the Codex bar" with a secret or balance value.
KindleVibe discovers Codex usage automatically from the local session files that Codex already writes.

The lookup logic is entirely local:

1. Recursively walk both directories and collect every `*.jsonl` session file.
2. Sort those files by modification time, newest first.
3. Read each file line by line as JSON.
4. Keep only events where:
   - `type == "event_msg"`
   - `payload.type == "token_count"`
   - the payload includes non-zero rate-limit windows
5. Keep the last matching rate-limit payload found in that file.
6. Stop at the first recent file that contains a valid rate-limit payload.

From that payload, KindleVibe renders:

- a primary bar
- a secondary bar
- optional detail text for `plan_type` and credits

If the session payload includes credit information, KindleVibe also shows the current Codex credit balance.
If that information is missing from the local session data, KindleVibe leaves the detail line partial rather than failing the page.

The labels are normalized for common Codex windows:

- `300` minutes becomes `5h Limit`
- `10080` minutes becomes `Weekly Limit`
- any other window becomes `<label> (<minutes>m)`

Reset time comes from:

- `resets_at`, if present
- otherwise `resets_in_seconds` added to the current local time

If you have not used Codex locally yet, or none of the recent session files contain a valid `token_count` event, the Codex section will render without usage bars instead of crashing the page.
The usual fix is simply to use Codex locally once so it writes fresh session data, then refresh the dashboard.

### Exchange Rates

KindleVibe also fetches a small set of reference exchange rates for the footer section:

- `BTC/USD`
- `USD/CNY`
- `AUD/CNY`

The fetch order is:

1. `https://cdn.jsdelivr.net/npm/@fawazahmed0/currency-api@latest/v1/currencies/<base>.json`
2. `https://latest.currency-api.pages.dev/v1/currencies/<base>.json`

The server requests one base-currency payload per needed base, formats the matching quote values for display, and logs any errors instead of failing the whole page.

KindleVibe's exchange-rate integration is inspired by [exchange-api](https://github.com/fawazahmed0/exchange-api), which uses GitHub Actions to generate a free currency-rate API.

## Security

- The app is intended for local-network use.
- This repository does not need committed credentials.
- Copilot tokens are read from your local environment or local GitHub Copilot app state.
- Codex usage is read from local session files only.
- `config.yaml` should stay free of secrets so the repository can be published safely.

> ⚠️ **AI Development Notice**: This project was heavily developed using AI tools including [Claude Code](https://claude.ai/code) and [Windsurf](https://windsurf.com/refer?referral_code=bfc775d405). It brings potential legal and ethical concerns, so use it with caution and always review the generated code.

![Dolores was privately chatting with Arnold. - HBO/Westworld](https://github.com/lexrus/AppAboutView/raw/main/screenshots/ai_injected_photo.webp)

> "You’re not in control, Dolores. You’re just playing your part."
> -- Bernard Lowe, WestWorld S2E2

## Repository Notes

- `config.yaml` is a sample config and is safe to commit.
- The built `kindlevibe` binary should not be committed.
- Exchange-rate data source attribution: [fawazahmed0/exchange-api](https://github.com/fawazahmed0/exchange-api)

## If you like this project

If you find AppAboutView useful, please consider checking out my other apps:

### [SubList](https://apps.apple.com/app/id6757860829) (iOS, iPadOS, macOS)

Track subscriptions, renewals, and spending in one place with reminders, analytics, and iCloud sync.

### [SwiftyMenu](https://apps.apple.com/app/id1567748223) (macOS)

A Finder extension which presents a customizable menu to rapidly open selected folders or files with your favorite applications.

### [Sharptooth](https://apps.apple.com/app/id6748440814) (macOS)

Effortlessly manage your Bluetooth devices right from the menu bar with custom hotkeys and smart automation.

## License

This project is available under the [WTFPL license](https://en.wikipedia.org/wiki/WTFPL).
