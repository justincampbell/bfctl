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
main.go                — subcommand dispatch + per-command flag parsing
internal/fc/fc.go      — port discovery + CLI session (Open / Run / Close)
internal/dump/dump.go  — parse craft_name, board_name, etc. out of a `diff all` body
```

`internal/dump/dump_test.go` carries an inline synthetic `diff all` fixture. Real captured dumps from the user's drones are gitignored (`air65.txt`, `BTFL_cli_backup_*.txt`) — do not check them in.

Subcommands: `backup`, `dump`, `get`, `info`, `ports`, `version`. Read-only for now; `cli`, `set`, and `restore` are reserved for future work.

## Talking to the FC — non-obvious bits

- **Identification**: USB CDC ACM with VID `0x0483` / PID `0x5740`. Auto-detect via `enumerator.GetDetailedPortsList`; fall back to `--port`.
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
