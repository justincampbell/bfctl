# Changelog

## Unreleased

- Initial release.
- `backup` — save full FC configuration to a file using the Configurator filename convention.
- `cli` — open an interactive Betaflight CLI session. On a TTY: line editing, up/down history (persisted to `~/.bfctl_history`), tab completion of top-level commands harvested from `help`. On a pipe: plain stdin/stdout. `exit`/`quit`/`q`/Ctrl-D close the session locally without rebooting the FC; `bfctl exec exit` if you actually want the FC to reboot.
- `diff` — print non-default settings (the FC's `diff all`).
- `dump` — print every setting (the FC's `dump all`, including defaults). **Breaking:** in earlier development builds `dump` produced the Configurator backup-file format (a `defaults nosave` prefix on top of `diff all`). That format now lives only in `bfctl backup`. Use `bfctl diff` for the equivalent of the old `dump` payload.
- `exec` — send one CLI command verbatim and print the reply. Catch-all for any FC CLI command without a dedicated subcommand. Tolerates the post-write disconnect from reboot-causing commands (`save`, `exit`, `bl`, `factory_reset`).
- `get` — print a single setting's value.
- `info` — print FC metadata (board, MCU, firmware, craft, pilot). Falls back to the `# name:` / `# pilot:` header lines on firmware that omits `set craft_name`/`set pilot_name`. `--json` supported.
- `ports` — list detected Betaflight FCs. Matches both STM32 (`Betaflight …` Product, VID `0x0483`) and AT32 (`AT32 Virtual Com Port` Product, VID `0x2E3C`) targets. `--json` supported.
- `set` — write a setting and (by default) `save`. `--no-save` opts out.
