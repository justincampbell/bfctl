# Changelog

## 0.2.0

### Added

- `cli` — interactive Betaflight CLI session. On a TTY: line editing, up/down history (persisted to `~/.bfctl_history`), tab completion of top-level commands harvested from `help`. On a pipe: plain stdin/stdout. `exit`/`quit`/`q`/Ctrl-D close the session locally without rebooting the FC; `bfctl exec exit` if you actually want the FC to reboot.
- `craft` — print the lowercase craft name on stdout. MSP-only, so it composes with subsequent `bfctl msp …` calls without a power-cycle. Exits 6 if `craft_name` is empty.
- `diff` — print non-default settings (the FC's `diff all`).
- `exec` — send one CLI command verbatim and print the reply. Catch-all for any FC CLI command without a dedicated subcommand. Tolerates the post-write disconnect from reboot-causing commands (`save`, `exit`, `bl`, `factory_reset`).
- `msp` — query Betaflight MSP commands directly, before CLI mode is entered. `bfctl msp` scans codes 1–199, skipping a denylist of every "in message" writer in that range (a 0-byte payload to a writer can corrupt config or, in the case of `MSP_SET_OSD_CANVAS`, brick the FC). `bfctl msp <code|name>` runs a single query and bypasses the denylist for opt-in research. Decoded output for the common identification/box/PID codes; raw hex otherwise. Handles MSP v1 jumbo frames. Surfaces a hint when the FC is stuck in CLI mode from a prior session. `--json` and `--timeout` supported.
- `restore` — replay a Configurator/`bfctl backup` file. Sends line-by-line with the same 15 ms / 100 ms delay schedule Configurator uses, appends `batch end`, then by default appends `save` (which reboots the FC into the new config). `--no-save` writes to RAM only; `--dry-run` prints the line stream. Auto-injects `set craft_name = …` / `set pilot_name = …` when the file's only record of those values is the AT32-style `# name:` / `# pilot:` headers.
- `set` — write a setting and (by default) `save`. `--no-save` opts out.

### Changed

- **Breaking:** `dump` now wraps the FC's `dump all` (every setting, including defaults). In earlier 0.1.0-era builds it produced the Configurator backup-file format (a `defaults nosave` prefix on top of `diff all`); that format now lives only in `bfctl backup`. Use `bfctl diff` for the equivalent of the old `dump` payload.
- **Breaking:** `info` is now MSP-only and no longer enters CLI mode, so it composes with subsequent `bfctl msp …` calls. The `pilot_name` field is consequently always empty (no v1 MSP exposes it); use `bfctl get pilot_name` if you need it.
- `backup --out` accepts three forms: an existing directory (auto-named inside), a file path (used verbatim, parent dirs created if needed), or `-` (stdout, pipe-friendly).
- `backup` default filename uppercases craft and board to match Configurator output.
- `backup` output always ends with exactly one `\n` so line-oriented tools and `git diff` don't complain.
- `ports` matches AT32 FCs (`AT32 Virtual Com Port`, VID `0x2E3C`) in addition to STM32.

## 0.1.0

- Initial release.
- `backup` — save full FC configuration to a file using the Configurator filename convention.
- `dump` — print full configuration to stdout.
- `get` — print a single setting's value.
- `info` — print FC metadata (board, MCU, firmware, craft, pilot). `--json` supported.
- `ports` — list detected Betaflight FCs. `--json` supported.
