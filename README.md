# bfctl

A CLI for talking to a [Betaflight](https://github.com/betaflight/betaflight) flight controller over USB.

`bfctl` does what the web Configurator's "Save to file" button does, but from a terminal. Designed for humans and AI coding agents: stdout is data, stderr is chatter, exit codes are stable, `--json` where it makes sense.

## Installation

```sh
brew install justincampbell/tap/bfctl       # Homebrew
go install github.com/justincampbell/bfctl@latest   # Go
```

Or grab a binary from [GitHub Releases](https://github.com/justincampbell/bfctl/releases).

## Usage

Plug the FC in via USB and run:

```sh
bfctl backup                   # save full config (Configurator-style filename)
bfctl backup --out my.txt      # …to a specific file path
bfctl backup --out -           # …to stdout
bfctl restore backup.txt       # replay a backup file (reboots after save)

bfctl info                     # FC metadata (board, MCU, firmware, craft)
bfctl info --json              # …as JSON
bfctl ports                    # list detected FCs
bfctl craft                    # print lowercase craft name only

bfctl diff                     # non-default settings
bfctl dump                     # every setting, including defaults
bfctl get craft_name           # one setting
bfctl set pilot_name = Maverick   # write + save (reboots)
bfctl exec status              # send any CLI command verbatim
bfctl cli                      # interactive REPL (history, tab completion)

bfctl msp                      # scan supported MSP codes
bfctl msp boxnames             # query one code (by name or number)
```

Pass `--port` if more than one FC is plugged in.

## Safety: `bfctl msp` scans

The scan skips a denylist of every "in message" MSP writer in range. Betaflight doesn't bounds-check `sbufReadU8`, so a 0-byte payload to a writer can silently corrupt config — and `MSP_SET_OSD_CANVAS` (188) has bricked an FC outright (DFU re-flash to recover). A scan can't read anything useful from a writer anyway. Single-code queries like `bfctl msp 188` bypass the denylist for opt-in research.

`bfctl msp` requires the FC to be in MSP mode (freshly booted). If you've run `bfctl set` / `bfctl exec` / `bfctl cli` since power-up, power-cycle before running MSP queries.

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Generic error |
| 2 | Usage error |
| 3 | No Betaflight FC found |
| 4 | More than one FC found (use `--port`) |
| 5 | Port already in use (Chrome / Configurator likely holding it) |
| 6 | Requested key not present (`get`, `craft`) |

## Troubleshooting

`no data` from the FC is usually a stale Chrome WebSerial lock from the web Configurator tab. Quit Chrome and retry.

## License

[MIT](LICENSE)
