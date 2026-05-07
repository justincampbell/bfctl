# bfctl

A Go CLI for talking to a [Betaflight](https://github.com/betaflight/betaflight) flight controller over USB CDC ACM. Replaces the manual "Save to file" workflow in the web Configurator at `app.betaflight.com`.

## Workflow rules

**Run `make install` after every change.** The user runs the binary directly between turns; the installed copy needs to be current. After any edit to Go source, the Makefile, the goreleaser config, or anything else that affects the built binary, run `make install` and verify it succeeds before reporting the change as done. (`make install` puts `bfctl` and `bfctl@<version>` on `$GOBIN`, so the version lifts directly from `git describe`.)

**Run `make lint` locally before pushing.** Use the Makefile target — never `go vet` alone — so it matches what CI runs (`golangci-lint`). CI failures on lint are avoidable; catch them locally.

If the change is documentation-only (`README.md`, `CHANGELOG.md`, etc.), `make install` and `make lint` are not required.

## Tech stack

- **Language**: Go (module `github.com/justincampbell/bfctl`)
- **Serial**: `go.bug.st/serial` and its `enumerator` subpackage
- **CLI**: stdlib `flag` (one `flag.NewFlagSet` per subcommand). No cobra/viper.
- **Build/release**: goreleaser → multi-arch binaries + Homebrew tap (`justincampbell/homebrew-tap`)

## Architecture

```
main.go                       — subcommand dispatch + per-command flag parsing
internal/fc/fc.go             — port discovery + CLI session (Open / Run / Close)
internal/fc/msp_session.go    — port-only MSP session (OpenMSP / Query / Close), no `#`
internal/msp/msp.go           — MSP v1 framing codec (Request / ReadResponse), supports jumbo
internal/msp/names.go         — MSP code → name table + Decode() for known reply shapes
internal/dump/dump.go         — parse craft_name, board_name, etc. out of a `diff all` body
```

`internal/dump/dump_test.go` carries an inline synthetic `diff all` fixture. Real captured dumps from the user's drones are gitignored (`air65.txt`, `BTFL_cli_backup_*.txt`) — do not check them in.

Subcommands: `backup`, `cli`, `diff`, `dump`, `exec`, `get`, `info`, `msp`, `ports`, `restore`, `set`, `version`.

`diff` and `dump` are thin wrappers around the FC's commands of the same name. `bfctl diff` runs `diff all` (non-defaults only); `bfctl dump` runs `dump all` (everything). Both print the FC's reply verbatim — neither prepends `defaults nosave`. That backup-file convention lives in `formatBackup`, used only by `bfctl backup`. Naming rule: when a subcommand maps 1:1 onto an FC CLI command, its name should match the FC's name; otherwise call it something else and document the deviation here.

`set` writes a single CLI `set` line to the FC and (by default) follows it with `save`. `save` reboots the FC — that is the only way Betaflight persists configuration changes. `--no-save` opts out for cases where you want to chain multiple sets manually.

`exec` is the catch-all for any other CLI command (`status`, `feature SOFTSERIAL`, `save`, `defaults`, …). It writes one line, prints the reply, and tolerates the post-write disconnect that comes with reboot-causing commands (save/exit/bl/factory_reset). It does **not** parse the reply or auto-save — that's a job for dedicated subcommands. Many "alias-shaped" subcommands (anything that's just `send fixed CLI line, print reply`) deliberately don't get promoted; if you reach for one, ask whether `bfctl exec <fc command>` would do.

`restore` replays a Configurator/`bfctl backup` file onto the FC, line by line with a 15 ms inter-line delay (100 ms for `profile`/`rateprofile` selectors) — matching `LINE_DELAY_MS` / `PROFILE_COMMAND_DELAY_MS` in Configurator's `useMspCliSession.js`. Bulk-writing the whole file in one shot was tried first and reliably stalled mid-batch on USB CDC flow control (FC's RX FIFO fills, host throttles, gap exceeds any reasonable silence threshold). `restore` appends `batch end` and (unless `--no-save`) `save`. It also injects `set craft_name = …` / `set pilot_name = …` when the file has `# name:` / `# pilot:` headers but no matching `set` lines — needed because AT32 firmware (Betaflight 2025.12.x) emits the names header-only and a naive replay drops them. `--dry-run` prints the line stream we would send.

`msp` is the MSP-mode counterpart to `exec`: it speaks the binary protocol the FC uses before `#` switches it to CLI. `bfctl msp` (no args) scans codes 1..MaxScanCode and prints every reply the FC accepts; `bfctl msp <code|name>` queries one specific code. **The FC must be in MSP mode** — i.e. freshly booted. After any `bfctl set` / `bfctl exec` / `bfctl cli` that didn't reboot, the FC stays in CLI mode and MSP queries fail; `MSPSession.Query` detects the echo and returns `ErrMSPInCLIMode` with a hint to power-cycle. The codec handles MSP v1 jumbo frames (`$M> 0xFF code size_lo size_hi …`) — Betaflight uses jumbo for any reply > 254 bytes (BOXNAMES, PIDNAMES, large config blocks). v2 (`$X<` preamble, 16-bit codes) is intentionally not implemented — every code bfctl needs has a v1 mapping.

`cli` is the interactive variant of `exec`: open the session once, read a line from stdin, send it, print the reply, repeat. Built on `(*Session).RunWith` with a snappier 400 ms idle window so each command doesn't pay the 1.5 s end-of-reply wait that `Run` uses for `diff all`. On a TTY, line editing comes from `github.com/peterh/liner`: up/down history (persisted to `~/.bfctl_history`), tab completion of top-level commands (harvested by parsing `help` output once at startup; falls back to a hardcoded list if the parse yields too few entries), and Ctrl-C cancels the current line. The prompt is `bf> ` (not `# ` — `# ` collides with how Betaflight prefixes comment lines and looked like commented-out shell in scrollback). On a pipe stdin, plain `bufio.Scanner` is used. `exit`/`quit`/`q` and Ctrl-D all close the session locally without rebooting the FC; if a user wants the actual FC `exit` (which reboots), `bfctl exec exit` is the path.

## Talking to the FC — non-obvious bits

- **Identification**: USB CDC ACM. Auto-detect via `enumerator.GetDetailedPortsList`, then filter through the `fcMatchers` table in `internal/fc/fc.go`. Each row pairs a USB Product substring with a VID:PID — a port matches if either signal hits. Both are needed because `go.bug.st/serial` on macOS returns the Product field for STM32 boards but leaves it empty for AT32 boards (Linux/Windows return both). To support a new MCU family, add one row to `fcMatchers`. Fall back to `--port`.
- **CLI mode entry needs a priming MSP frame.** Sending `#` cold is silently ignored — the FC's USB-side parser must be activated by a valid MSP exchange first. `fc.Open` sends MSP_API_VERSION (cmd 1) and looks at the reply: a `$M>` response means we're in MSP mode and can now send `#`; an echo of our MSP bytes (`$M<` or a `# ` prompt) means we're already in CLI from a prior session that closed without exiting. Both paths are handled.
- **Single-byte `#`, no CR/LF.** This matches what the web Configurator does (`src/composables/useCli.js`).
- **Never send `exit`.** It reboots the FC. Just close the port.
- **Response framing**: silence-detection (1.5s of no bytes after the last chunk) marks the end of a `diff all` reply. The Configurator uses STX/ETX framing on each command — silence works fine for `diff all` and avoids the extra layer; consider STX/ETX if a future command turns out to be flaky.
- **Trailing `save` line**: `diff all` ends with `# save configuration\nsave\n# `. The literal `save` line is a hint, not part of the dump — `cleanResponse` strips it (Configurator does the same).
- **`defaults nosave` prefix**: the Configurator-style backup file starts with `defaults nosave\n\n\n# version\n…`. The FC does not emit `defaults nosave`; `formatBackup` in `main.go` prepends it so the backup is a self-contained restore script.

## Agent-friendly conventions

- **stdout = data, stderr = humans.** Errors, hints, and progress go to stderr.
- **Stable exit codes.** `0` ok, `1` generic, `2` usage, `3` no FC, `4` multiple FCs, `5` port in use, `6` `get` key not found.
- **`--json`** on `info` and `ports`.
- **No interactive prompts.** Every decision is a flag.
- **`--port` is available on every command** that talks to the FC.
- **Stable error phrasing.** Other tools (and agents) may pattern-match.

## Maintaining this file

Keep this file current. Update it when:
- A subcommand is added, removed, or has its semantics changed
- A new non-obvious FC quirk is discovered
- An exit code is added or its meaning shifts
- The release/install workflow changes

Don't document speculative features. Reserved future subcommands (`cli`, `set`, `restore`) get a one-line mention so naming collisions are avoided, nothing more.

## Releasing

Tags `v*` trigger `.github/workflows/release.yml`, which runs goreleaser to publish multi-arch binaries on GitHub Releases and update the Homebrew formula in `justincampbell/homebrew-tap`. Goreleaser needs `HOMEBREW_TAP_GITHUB_TOKEN` in CI secrets (set once per repo).

Local snapshot build: `make build` (versioned binary copy is created alongside).
