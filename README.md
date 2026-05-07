# bfctl

A CLI for talking to a [Betaflight](https://github.com/betaflight/betaflight) flight controller over USB.

`bfctl` does what the web Configurator's "Save to file" button does, but from a terminal — no browser, no clicking, scriptable. It's designed to be friendly to both humans and AI coding agents: stdout is data, stderr is chatter, exit codes are stable, and `--json` is available where structured output makes sense.

## Installation

### Homebrew

```sh
brew install justincampbell/tap/bfctl
```

### Go

```sh
go install github.com/justincampbell/bfctl@latest
```

### Binary releases

Download a prebuilt binary from the [GitHub Releases](https://github.com/justincampbell/bfctl/releases) page.

## Usage

Plug the FC in via USB and run:

```sh
bfctl backup                 # save full config to BTFL_cli_backup_<craft>_<ts>_<board>.txt
bfctl diff                   # print non-default settings (FC's `diff all`)
bfctl dump                   # print every setting (FC's `dump all`, including defaults)
bfctl get craft_name         # print one setting
bfctl info                   # print FC metadata
bfctl info --json            # …as JSON
bfctl ports                  # list detected FCs
bfctl set pilot_name = Maverick   # write a setting and save (reboots the FC)
bfctl set pilot_name = Maverick --no-save   # …or set without persisting
bfctl restore backup.txt          # replay a Configurator-format backup (reboots after save)
bfctl restore backup.txt --no-save   # …apply to RAM only (lost on next reboot)
bfctl restore backup.txt --dry-run   # …print the line stream that would be sent
bfctl exec status                # any other CLI command, reply printed verbatim
bfctl exec feature SOFTSERIAL    # toggle a feature
bfctl cli                        # interactive Betaflight CLI session (history + tab completion)
bfctl msp                        # scan every supported MSP command and print replies
bfctl msp boxnames               # query one MSP code (by name or number) and decode it
bfctl msp 116 --json             # …same, JSON output
```

`bfctl msp` requires the FC to be in MSP mode (freshly booted). If you've already run `bfctl set` / `bfctl exec` / `bfctl cli` since power-up, the FC stays in CLI mode — power-cycle before running MSP queries.

The default scan only probes codes 1–99. The 131–199 range contains at least one code that bricks current Betaflight (observed on the LIONBEE_V1 — DFU re-flash required to recover). To scan further use `--max` explicitly and at your own risk.

If more than one FC is plugged in, pass `--port`:

```sh
bfctl backup --port /dev/cu.usbmodem2070378831451
```

`backup` writes to the current directory by default; use `--out` to choose another:

```sh
bfctl backup --out backups/
```

## Exit codes

Stable exit codes for scripting and agent use:

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Generic error |
| 2 | Usage error |
| 3 | No Betaflight FC found |
| 4 | More than one FC found (use `--port`) |
| 5 | Port already in use (Chrome / Configurator likely holding it) |
| 6 | `get`: requested key is not present in the dump |

## How it works

The FC enumerates as a USB CDC ACM device. `bfctl` matches on the USB Product string (`Betaflight …` for STM32 targets, `AT32 Virtual Com Port` for Artery AT32 targets) — and falls back to VID:PID (`0483:5740` for STM32, `2E3C:5740` for AT32) on platforms that don't surface the Product field. A single binary covers both MCU families. `bfctl` opens the matching `/dev/cu.usbmodem*`, sends `#\r\n` to enter CLI mode, runs `diff all`, and captures the output. It does **not** send `exit` (which would reboot the FC) — it just closes the port.

If you see "no data" errors, the most common cause is a stale Chrome WebSerial lock from the web Configurator tab. Quit Chrome and retry.

## License

[MIT](LICENSE)
