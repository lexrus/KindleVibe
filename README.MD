# KindleVibe

KindleVibe is a small local Go server that shows GitHub Copilot, Codex, and Linear activity in a Kindle-friendly web page. It is designed for e-ink screens: high contrast, portrait-friendly layout, large typography, and automatic refresh.

![Demo](demo.jpg)

## Features

- **Kindle optimized:** Minimal, high-contrast UI for e-ink browsers.
- **Portrait dashboard:** Single-screen layout tuned for a 1080 x 1440 Kindle display.
- **Auto-refresh:** Refreshes every 15 seconds with a background XHR poll.
- **Current providers:** GitHub Copilot, Codex, Linear, and Gemini.
- **Exchange snapshot:** Configurable via `config.yaml` to display up to 2 columns of exchange rates at the bottom. Default includes BTC/USD, USD/CNY, AUD/CNY, JPY/CNY, CAD/CNY, and HKD/CNY.
- **Local-only:** Reads usage from local auth state and local session files.
- **Simple startup:** `make` rebuilds and starts the server.

## Prerequisites

- Go 1.21 or later.
- GitHub Copilot authenticated locally.
- Codex installed and used locally so session files exist.
- Gemini CLI installed and authenticated locally.

## Getting Started

1. **Clone the repository**

   ```bash
   git clone https://github.com/lexrus/KindleVibe.git
   cd KindleVibe
   ```

2. **Configure the server**

   Create a local config from the committed example:

   ```bash
   cp config.yaml.example config.yaml
   ```

   Then edit `config.yaml` to set the port, enabled agents, and optional tokens.

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

`config.yaml.example`:

```yaml
server:
  port: 8080
agents:
  copilot:
    enabled: true
    token: ""
  codex:
    enabled: true
  linear:
    enabled: true
    token: ""
  gemini:
    enabled: true
exchange:
  pairs:
    - base: "BTC"
      quote: "USD"
    - base: "USD"
      quote: "CNY"
    - base: "AUD"
      quote: "CNY"
    - base: "JPY"
      quote: "CNY"
    - base: "CAD"
      quote: "CNY"
    - base: "HKD"
      quote: "CNY"
```

`config.yaml` only controls which providers are shown and which port the server listens on.
Your local `config.yaml` can also include `agents.copilot.token` and `agents.linear.token`.
`config.yaml` is gitignored for local-only secrets.

## Data Sources

KindleVibe reads provider data when the `/` page is requested. For each enabled provider, `main.go` tries to build an `AgentStats` block and logs errors instead of failing the whole page, so one broken provider does not stop the dashboard from rendering.

### Refresh and Rate-Limit Thresholds

KindleVibe throttles upstream requests to avoid hammering external services on every page refresh:

| Provider | Min interval between requests |
|---|---|
| GitHub Copilot | 2 minutes |
| Codex | 2 minutes |
| Gemini | 2 minutes |
| Exchange rates | 10 minutes |

Between fetches the dashboard renders the last known values. Linear issues are always fetched fresh on each dashboard rebuild since they reflect live task state.

### GitHub Copilot

KindleVibe fetches Copilot usage from GitHub's internal Copilot endpoint using local auth state.

Token resolution order:

1. `agents.copilot.token` in `config.yaml`
2. `COPILOT_API_TOKEN`
3. `~/.config/github-copilot/apps.json`

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

You can keep the token either in `config.yaml` (`agents.copilot.token`) or in your shell environment (`COPILOT_API_TOKEN`).

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

### Linear

KindleVibe fetches your assigned Linear issues from the Linear GraphQL API and renders a tiny list for:

- `Todo`
- `In Progress`

Linear token resolution order:

1. `agents.linear.token` in `config.yaml`
2. `LINEAR_API_TOKEN`
3. `LINEAR_TOKEN`

Set a token in your shell before running `make`:

```bash
export LINEAR_API_TOKEN=your_linear_token_here
make
```

You can keep the Linear token in `config.yaml` (`agents.linear.token`) or in environment variables.

The Linear query is scoped to issues assigned to you (`isMe: true`) and fetches a small number of recent items per state so the list stays compact on Kindle screens.

### Exchange Rates

KindleVibe fetches reference exchange rates for the footer section, which can be configured under `exchange.pairs` in your `config.yaml`. By default, it displays:

- `BTC/USD`
- `USD/CNY`
- `AUD/CNY`
- `JPY/CNY`
- `CAD/CNY`
- `HKD/CNY`

The fetch order is:

1. `https://cdn.jsdelivr.net/npm/@fawazahmed0/currency-api@latest/v1/currencies/<base>.json`
2. `https://latest.currency-api.pages.dev/v1/currencies/<base>.json`

The server requests one base-currency payload per needed base, formats the matching quote values for display, and logs any errors instead of failing the whole page.

KindleVibe's exchange-rate integration is inspired by [exchange-api](https://github.com/fawazahmed0/exchange-api), which uses GitHub Actions to generate a free currency-rate API.

## Security

- The app is intended for local-network use.
- This repository does not need committed credentials.
- Copilot tokens are read from local `config.yaml`, local environment, or local GitHub Copilot app state.
- Codex usage is read from local session files only.
- Linear data is fetched with a token from local `config.yaml` or local environment.
- `config.yaml.example` is safe to commit; local `config.yaml` is gitignored.

> ⚠️ **AI Development Notice**: This project was heavily developed using AI tools including Codex and Copilot. It brings potential legal and ethical concerns, so use it with caution and always review the generated code.

![Dolores was privately chatting with Arnold. - HBO/Westworld](https://github.com/lexrus/AppAboutView/raw/main/screenshots/ai_injected_photo.webp)

> "You’re not in control, Dolores. You’re just playing your part."
> -- Bernard Lowe, WestWorld S2E2

## Repository Notes

- `config.yaml.example` is a sample config and is safe to commit.
- `config.yaml` is local-only and gitignored.
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
