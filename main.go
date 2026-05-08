// bfctl is a CLI for talking to a Betaflight flight controller over USB.
//
// stdout is data; stderr is for humans (progress, hints, errors). All
// commands accept --port to override auto-detection. Exit codes are
// stable so other tools (and agents) can branch on them.
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/peterh/liner"

	"github.com/justincampbell/bfctl/internal/dump"
	"github.com/justincampbell/bfctl/internal/fc"
	"github.com/justincampbell/bfctl/internal/msp"
)

var version = "dev"

// Stable exit codes. Agents may branch on these.
const (
	exitOK           = 0
	exitGeneric      = 1
	exitUsage        = 2
	exitNoFC         = 3
	exitMultipleFCs  = 4
	exitPortInUse    = 5
	exitNotFound     = 6 // get: key not present in dump
)

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}

	switch os.Args[1] {
	case "backup":
		os.Exit(cmdBackup(os.Args[2:]))
	case "cli":
		os.Exit(cmdCLI(os.Args[2:]))
	case "craft":
		os.Exit(cmdCraft(os.Args[2:]))
	case "diff":
		os.Exit(cmdDiff(os.Args[2:]))
	case "dump":
		os.Exit(cmdDump(os.Args[2:]))
	case "exec":
		os.Exit(cmdExec(os.Args[2:]))
	case "msp":
		os.Exit(cmdMSP(os.Args[2:]))
	case "get":
		os.Exit(cmdGet(os.Args[2:]))
	case "info":
		os.Exit(cmdInfo(os.Args[2:]))
	case "ports":
		os.Exit(cmdPorts(os.Args[2:]))
	case "restore":
		os.Exit(cmdRestore(os.Args[2:]))
	case "set":
		os.Exit(cmdSet(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("bfctl", version)
	case "help", "--help", "-h":
		printUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "bfctl: unknown command %q\n\n", os.Args[1])
		printUsage(os.Stderr)
		os.Exit(exitUsage)
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `bfctl — talk to a Betaflight flight controller over USB.

Usage:
  bfctl <command> [flags]

Commands:
  backup   Save full configuration to a file
  cli      Open an interactive Betaflight CLI session
  craft    Print the lowercase craft name (exit non-zero if unset)
  diff     Print non-default settings (`+"`diff all`"+`)
  dump     Print every setting (`+"`dump all`"+`, including defaults)
  exec     Send one CLI command verbatim and print the reply
  get      Print one setting's value
  info     Print FC metadata (board, firmware, craft, …)
  msp      Query Betaflight MSP commands (single code or full scan)
  ports    List detected Betaflight FCs
  restore  Replay a backup file onto the FC (reboots after save)
  set      Write a setting and persist it (reboots the FC)
  version  Print version

Run 'bfctl <command> --help' for command-specific flags.
`)
}

// ----- backup -----

// cmdBackup writes the FC's `diff all` body, wrapped in the Configurator
// backup-file format, to one of three destinations:
//
//   - `--out <dir>` (an existing directory, including the default `.`):
//     auto-named file inside that directory using the Configurator's
//     `BTFL_cli_backup_<CRAFT>_<ts>_<BOARD>.txt` convention.
//   - `--out <file>` (anything else): exact file path. Parent directories
//     are created if missing; existing file is overwritten.
//   - `--out -`: write to stdout. Nothing else is printed (useful for piping).
func cmdBackup(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	out := fs.String("out", ".", "output destination: directory, file path, or `-` for stdout")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	body, _, err := pullDump(*port, "diff all")
	if err != nil {
		return reportFCErr(err)
	}

	payload := []byte(formatBackup(body))

	if *out == "-" {
		if _, err := os.Stdout.Write(payload); err != nil {
			fmt.Fprintln(os.Stderr, "bfctl:", err)
			return exitGeneric
		}
		return exitOK
	}

	path := resolveBackupPath(*out, dump.Parse(body), time.Now())
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "bfctl:", err)
			return exitGeneric
		}
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "bfctl:", err)
		return exitGeneric
	}
	fmt.Println(path)
	return exitOK
}

// resolveBackupPath turns an --out value into the file path to write. If out
// names an existing directory, the Configurator-style auto-generated filename
// is joined onto it; otherwise out is the destination path verbatim.
func resolveBackupPath(out string, info dump.Info, when time.Time) string {
	if fi, err := os.Stat(out); err == nil && fi.IsDir() {
		return filepath.Join(out, backupFilename(info, when))
	}
	return out
}

// formatBackup wraps a raw `diff all` body into the file format the web
// Configurator produces: a leading `defaults nosave` so the file is a
// self-contained restore script, two blank separator lines, then the body,
// and exactly one trailing newline (POSIX text-file convention so line-
// oriented tools and `git diff` don't complain).
func formatBackup(body string) string {
	if !strings.HasPrefix(body, "\n") {
		body = "\n" + body
	}
	body = strings.TrimRight(body, "\n")
	return "defaults nosave\n\n" + body + "\n"
}

// backupFilename matches the convention used by app.betaflight.com so saved
// files interleave naturally:
//
//	BTFL_cli_backup_<CRAFT>_<YYYYMMDD>_<HHMMSS>_<BOARD>.txt
//
// Configurator uppercases the craft slug — a mixed-case craft name like
// "LionBee3" lands as "LIONBEE3" — so we do too. Board names already come
// uppercased from the FC, but we ToUpper defensively to cover any future
// firmware that doesn't.
func backupFilename(info dump.Info, when time.Time) string {
	craft := strings.ToUpper(sanitize(info.CraftName))
	if craft == "" {
		craft = "UNKNOWN"
	}
	board := strings.ToUpper(sanitize(info.Board))
	if board == "" {
		board = "UNKNOWN"
	}
	return fmt.Sprintf("BTFL_cli_backup_%s_%s_%s.txt",
		craft, when.Format("20060102_150405"), board)
}

// sanitize keeps a string safe for use in a filename.
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('_')
		}
	}
	return b.String()
}

// ----- diff / dump -----
//
// `bfctl diff` runs the FC's `diff all` (only non-default settings).
// `bfctl dump` runs the FC's `dump all` (every setting, including defaults).
// Both print the FC's reply verbatim. The `defaults nosave` prefix that
// Configurator adds when saving a backup file lives only in `formatBackup`,
// which `bfctl backup` calls — neither diff nor dump wrap their output.

func cmdDiff(args []string) int {
	return runShowCmd("diff", "diff all", args)
}

func cmdDump(args []string) int {
	return runShowCmd("dump", "dump all", args)
}

// runShowCmd is the shared body of `bfctl diff` and `bfctl dump`: parse
// flags, pull the body via the given CLI command, print it.
func runShowCmd(name, cliCmd string, args []string) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	body, _, err := pullDump(*port, cliCmd)
	if err != nil {
		return reportFCErr(err)
	}
	fmt.Print(body)
	if !strings.HasSuffix(body, "\n") {
		fmt.Println()
	}
	return exitOK
}

// ----- cli -----

// cmdCLI opens an interactive Betaflight CLI session: read a line from
// stdin, send it via Session.RunWith with REPL-tuned timeouts, print the
// reply, loop until EOF or a quit/exit/q line.
//
// `exit` is treated as a REPL-quit signal, not forwarded to the FC. Sending
// `exit` to Betaflight reboots it, which is almost never what someone typing
// into a REPL wants. Anyone who actually wants to reboot can do
// `bfctl exec exit`.
//
// Reboot-causing commands the FC accepts (`save`, `bl`, `factory_reset`,
// `defaults`) tear the USB device down mid-reply. RunWith returns the
// partial reply along with the read error; cli prints what it got, notes
// the disconnect on stderr, and exits 0.
//
// On a TTY, line editing (history via up/down arrow, tab completion of
// top-level commands, Ctrl-C to cancel a line) is provided by peterh/liner.
// History persists across sessions in ~/.bfctl_history. On a pipe the path
// degrades to a plain bufio.Scanner — fewer features, but `printf … | bfctl
// cli` and shell redirection both still work.
func cmdCLI(args []string) int {
	fs := flag.NewFlagSet("cli", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: bfctl cli")
		return exitUsage
	}

	path, err := fc.Resolve(*port)
	if err != nil {
		return reportFCErr(err)
	}
	sess, err := fc.Open(path)
	if err != nil {
		return reportFCErr(err)
	}
	defer func() { _ = sess.Close() }()

	// Prompt rendering and line editing are gated on stdout: piping input
	// in is fine, but redirecting output should keep stdout free of "# "
	// lines and skip the raw-mode terminal entirely.
	if isCharDevice(os.Stdout) {
		return runCLIInteractive(sess, path)
	}
	return runCLIPiped(sess)
}

// runCLIPiped is the non-TTY path: a plain bufio.Scanner, no prompts, no
// history, no completion. Suitable for `printf '…' | bfctl cli` and for
// scripts that pipe a sequence of commands.
func runCLIPiped(sess *fc.Session) int {
	scanner := bufio.NewScanner(os.Stdin)
	// Default 64 KB buffer truncates anything resembling a `diff all`
	// paste. Match the dump parser's 1 MB ceiling.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		cont, _ := dispatchCLILine(sess, scanner.Text())
		if !cont {
			break
		}
	}
	return exitOK
}

// runCLIInteractive is the TTY path. peterh/liner provides:
//   - up/down arrow → history (in-memory + persisted to ~/.bfctl_history)
//   - tab → complete top-level command names harvested from `help`
//   - Ctrl-C → cancel the current line and re-prompt (loop continues)
//   - Ctrl-D on an empty line → EOF, exit cleanly
func runCLIInteractive(sess *fc.Session, path string) int {
	l := liner.NewLiner()
	defer func() { _ = l.Close() }()
	l.SetCtrlCAborts(true)

	cmds := harvestCLICommands(sess)
	l.SetCompleter(func(line string) []string {
		return cliCompletions(line, cmds)
	})

	historyPath := cliHistoryPath()
	if historyPath != "" {
		if f, err := os.Open(historyPath); err == nil {
			_, _ = l.ReadHistory(f)
			_ = f.Close()
		}
	}

	fmt.Fprintf(os.Stderr, "bfctl: connected to %s — type 'quit' or Ctrl-D to disconnect\n", path)

	rc := exitOK
LOOP:
	for {
		input, err := l.Prompt("bf> ")
		switch {
		case err == io.EOF:
			fmt.Fprintln(os.Stderr)
			break LOOP
		case errors.Is(err, liner.ErrPromptAborted):
			// Ctrl-C: discard the current line, prompt again.
			continue
		case err != nil:
			fmt.Fprintln(os.Stderr, "bfctl: prompt:", err)
			rc = exitGeneric
			break LOOP
		}
		if strings.TrimSpace(input) != "" {
			l.AppendHistory(input)
		}
		cont, _ := dispatchCLILine(sess, input)
		if !cont {
			break
		}
	}

	if historyPath != "" {
		if f, err := os.Create(historyPath); err == nil {
			_, _ = l.WriteHistory(f)
			_ = f.Close()
		}
	}
	return rc
}

// dispatchCLILine processes one line of input. Returns (continueLoop,
// disconnect): continueLoop is false when the user typed quit/exit/q or the
// FC disconnected mid-reply (reboot-causing command).
func dispatchCLILine(sess *fc.Session, raw string) (cont, disconnected bool) {
	line := strings.TrimSpace(raw)
	// Tolerate paste-back of either prompt style: our own `bf> …` lines
	// or the Configurator's `# …` transcript format.
	for _, prefix := range []string{"bf> ", "# "} {
		if strings.HasPrefix(line, prefix) {
			line = strings.TrimSpace(line[len(prefix):])
			break
		}
	}
	if line == "" {
		return true, false
	}
	if line == "quit" || line == "q" || line == "exit" {
		return false, false
	}
	reply, err := sess.RunWith(line, 5*time.Second, 400*time.Millisecond)
	reply = strings.TrimRight(reply, " \t\r\n")
	if reply != "" {
		fmt.Println(reply)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bfctl:", err, "(FC disconnected)")
		return false, true
	}
	return true, false
}

// cliHistoryPath returns ~/.bfctl_history, or "" if the home dir can't be
// resolved (in which case we silently skip history persistence).
func cliHistoryPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bfctl_history")
}

// fallbackCLICommands is the static command list used when `help` parsing
// fails (FC disconnected, garbled output, etc.). Curated from a recent
// Betaflight build; not exhaustive.
var fallbackCLICommands = []string{
	"adjrange", "aux", "batch", "beacon", "beeper", "bind_rx", "bl",
	"board_name", "color", "defaults", "diff", "dma", "dump", "exit",
	"feature", "get", "help", "led", "manufacturer_id", "map", "mcu_id",
	"mixer", "mmix", "mode_color", "motor", "msc", "play_sound", "profile",
	"rateprofile", "resource", "rxfail", "rxrange", "save", "serial",
	"servo", "set", "smix", "status", "tasks", "timer", "version", "vtx",
	"vtxtable",
}

// harvestCLICommands runs `help` against the live FC and parses out the
// top-level command names. The Betaflight `help` output uses one indented
// line per command:
//
//	adjrange - configure adjustment ranges
//	    <index> <unused> <range channel> ...
//	aux - configure modes
//	    <index> <mode> <aux> <start> <end>
//
// Every non-indented line is a command; the first whitespace-separated
// token is its name. We tolerate noise: if fewer than 5 commands parse
// out, fall back to the static list rather than crippling completion.
func harvestCLICommands(sess *fc.Session) []string {
	body, err := sess.RunWith("help", 5*time.Second, 400*time.Millisecond)
	if err != nil || body == "" {
		return fallbackCLICommands
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue // argument hint line
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if !isCommandName(name) {
			continue
		}
		out = append(out, name)
	}
	if len(out) < 5 {
		return fallbackCLICommands
	}
	sort.Strings(out)
	return out
}

// isCommandName accepts only [a-z0-9_]+, which matches every Betaflight
// CLI command name and rejects punctuation, numbers, headers, etc.
func isCommandName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}

// cliCompletions returns prefix-matched candidates for the first word of
// the line. Multi-word completion (set <key>, get <key>) is intentionally
// not done yet — the full key list isn't cached and a per-tab dump is too
// slow.
func cliCompletions(line string, cmds []string) []string {
	if strings.ContainsAny(line, " \t") {
		return nil
	}
	var matches []string
	for _, c := range cmds {
		if strings.HasPrefix(c, line) {
			matches = append(matches, c)
		}
	}
	return matches
}

// isCharDevice reports whether f is connected to a terminal.
func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ----- exec -----

// cmdExec is the catch-all for Betaflight CLI commands that don't have a
// dedicated subcommand. Args after the flags are joined with a single space
// and sent verbatim, so quoting matches what you'd type in the Configurator
// CLI:
//
//	bfctl exec status
//	bfctl exec feature SOFTSERIAL
//	bfctl exec save
//
// Some commands (`save`, `exit`, `bl`, `factory_reset`) reboot the FC, which
// disconnects USB mid-reply. exec prints whatever was received before the
// disconnect and exits 0; the disconnect itself is logged to stderr so the
// caller still has a signal.
//
// exec is intentionally dumb — it does not parse the reply, does not save
// after the command, and does not assume success. Use `set` for the
// set+save flow; use `restore` (future) for batch replays.
func cmdExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: bfctl exec <cli command...>")
		return exitUsage
	}
	cmd := strings.Join(fs.Args(), " ")

	path, err := fc.Resolve(*port)
	if err != nil {
		return reportFCErr(err)
	}
	sess, err := fc.Open(path)
	if err != nil {
		return reportFCErr(err)
	}
	defer func() { _ = sess.Close() }()

	reply, err := sess.Run(cmd)
	reply = strings.TrimRight(reply, " \t\r\n")
	if reply != "" {
		fmt.Println(reply)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bfctl: read after write:", err, "(FC may have rebooted)")
	}
	return exitOK
}

// ----- get -----

func cmdGet(args []string) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: bfctl get <key>")
		return exitUsage
	}
	key := fs.Arg(0)

	body, _, err := pullDump(*port, "diff all")
	if err != nil {
		return reportFCErr(err)
	}

	val, ok := dump.Get(body, key)
	if !ok {
		fmt.Fprintf(os.Stderr, "bfctl: key %q not present in dump\n", key)
		return exitNotFound
	}
	fmt.Println(val)
	return exitOK
}

// ----- info -----

// cmdInfo prints FC metadata using MSP queries only — it never sends `#`
// to enter CLI mode. This matters because once the FC has been switched
// into CLI mode, all subsequent MSP queries fail until the FC reboots
// (MSP and CLI share one USB CDC parser, and there's no reverse switch).
// Earlier versions of `info` used the `diff all` CLI path to get every
// field in one shot; that left the FC in CLI mode and broke any later
// `bfctl msp` call until power-cycle.
//
// One regression vs the old CLI path: `pilot_name` is not exposed by any
// MSP v1 command (only MSP2_GET_TEXT, which bfctl doesn't speak). The
// JSON field stays for compatibility but is empty; the human output
// drops the line entirely. Use `bfctl get pilot_name` if you need it
// — and remember that command rebooots the MSP↔CLI boundary too.
func cmdInfo(args []string) int {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	path, err := fc.Resolve(*port)
	if err != nil {
		return reportFCErr(err)
	}
	info, err := fc.QueryInfo(path)
	if err != nil {
		return reportFCErr(err)
	}

	if *asJSON {
		out := struct {
			Port      string `json:"port"`
			Board     string `json:"board"`
			MCUID     string `json:"mcu_id"`
			Firmware  string `json:"firmware"`
			CraftName string `json:"craft_name"`
			PilotName string `json:"pilot_name"`
		}{path, info.Board, info.MCUID, info.Firmware, info.CraftName, info.PilotName}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return exitOK
	}

	fmt.Printf("Port:       %s\n", path)
	fmt.Printf("Board:      %s\n", info.Board)
	fmt.Printf("MCU ID:     %s\n", info.MCUID)
	fmt.Printf("Firmware:   %s\n", info.Firmware)
	fmt.Printf("Craft:      %s\n", info.CraftName)
	return exitOK
}

// ----- craft -----

// cmdCraft prints the FC's craft name, lowercased, on its own line. Exits
// non-zero with a clear stderr message if the FC has no craft name set.
//
// MSP-only (same path as `bfctl info`) so calling it doesn't switch the FC
// into CLI mode and break subsequent MSP queries. The point is to replace
// the `bfctl info -json | jq -r '.craft_name' | tr '[:upper:]' '[:lower:]'`
// shell incantation with a single command.
func cmdCraft(args []string) int {
	fs := flag.NewFlagSet("craft", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	path, err := fc.Resolve(*port)
	if err != nil {
		return reportFCErr(err)
	}
	info, err := fc.QueryInfo(path)
	if err != nil {
		return reportFCErr(err)
	}

	name := strings.ToLower(strings.TrimSpace(info.CraftName))
	if name == "" {
		fmt.Fprintln(os.Stderr, "bfctl: craft_name is not set on this FC")
		return exitNotFound
	}
	fmt.Println(name)
	return exitOK
}

// ----- restore -----

// cmdRestore replays a backup file onto the FC line by line, mirroring how
// Betaflight Configurator's CLI tab does it. Format expected: the
// Configurator/`bfctl backup` output — `defaults nosave` prefix, FC `diff
// all` body (with its own embedded `batch start`), no trailing `batch end`
// or `save`. We append `batch end` ourselves, and by default append a
// `save` (which reboots the FC into the new config). `--no-save` skips the
// save (changes live in RAM only, lost on next reboot).
//
// We send line-by-line with a small inter-line delay rather than as one
// bulk write because the FC's USB CDC RX buffer fills up on bulk transfers
// and the resulting flow-control pauses can run longer than any reasonable
// silence-detection threshold (observed on LIONBEE_V1: bulk write cut off
// at line ~23 of 61, FC kept processing in the background, leaving the
// next bfctl session colliding with leftover state). 15 ms per line, 100
// ms for `profile`/`rateprofile` selectors — same numbers Configurator
// uses (LINE_DELAY_MS, PROFILE_COMMAND_DELAY_MS in useMspCliSession.js).
func cmdRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	noSave := fs.Bool("no-save", false, "skip the trailing `save` (changes lost on next reboot)")
	dryRun := fs.Bool("dry-run", false, "print what would be sent; do not open the FC")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: bfctl restore <file>")
		return exitUsage
	}
	filePath := fs.Arg(0)
	body, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bfctl:", err)
		return exitGeneric
	}
	if !looksLikeBackup(body) {
		fmt.Fprintf(os.Stderr, "bfctl: %s does not look like a Betaflight backup (no `batch start` line)\n", filePath)
		return exitGeneric
	}

	lines := splitRestoreLines(body)
	// On AT32 firmware (and possibly others) `diff all` emits `# name: X`
	// / `# pilot: Y` headers but *not* the corresponding `set craft_name`
	// / `set pilot_name` lines. Without us supplementing, restoring leaves
	// craft and pilot at firmware defaults — a silent backup/restore data
	// loss. Inject the missing `set` lines.
	lines = injectMissingNameSets(lines)
	// Append batch end if the file doesn't already have one (Configurator
	// backups never do).
	if !linesContain(lines, "batch end") {
		lines = append(lines, "batch end")
	}

	if *dryRun {
		for _, line := range lines {
			fmt.Println(line)
		}
		if !*noSave {
			fmt.Println("save")
		}
		return exitOK
	}

	path, err := fc.Resolve(*port)
	if err != nil {
		return reportFCErr(err)
	}
	sess, err := fc.Open(path)
	if err != nil {
		return reportFCErr(err)
	}
	defer func() { _ = sess.Close() }()

	const (
		lineDelay    = 15 * time.Millisecond
		profileDelay = 100 * time.Millisecond
	)
	var replies strings.Builder
	for _, line := range lines {
		if err := sess.WriteLine(line); err != nil {
			fmt.Fprintln(os.Stderr, "bfctl: write:", err)
			return exitGeneric
		}
		delay := lineDelay
		if isProfileSelector(line) {
			delay = profileDelay
		}
		time.Sleep(delay)
		// Drain whatever the FC has emitted so the kernel/USB CDC buffer
		// doesn't back up. Short window — we don't *wait* for a reply, we
		// just take whatever's there.
		replies.WriteString(sess.DrainAvailable(0))
	}
	// Final drain: give the FC time to finish processing batch end and
	// emit any deferred error messages (Betaflight reports batch errors
	// only at batch end).
	replies.WriteString(sess.DrainAvailable(2 * time.Second))

	reply := strings.TrimRight(replies.String(), " \t\r\n")
	if reply != "" {
		fmt.Println(reply)
	}
	if mentionsRestoreError(reply) {
		fmt.Fprintln(os.Stderr, "bfctl: FC reported errors during restore (see output above)")
		return exitGeneric
	}

	if *noSave {
		fmt.Fprintln(os.Stderr, "bfctl: --no-save: changes will be lost on next reboot")
		return exitOK
	}
	if err := sess.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "bfctl: save:", err)
		return exitGeneric
	}
	fmt.Fprintln(os.Stderr, "bfctl: restored (FC is rebooting)")
	return exitOK
}

// splitRestoreLines normalises line endings and drops the trailing empty
// element that strings.Split produces when the file ends in a newline.
func splitRestoreLines(body []byte) []string {
	s := strings.ReplaceAll(string(body), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// linesContain checks whether any non-comment line equals want (after
// trimming).
func linesContain(lines []string, want string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// injectMissingNameSets reads the file's `# name:` and `# pilot:` header
// lines and appends `set craft_name = …` / `set pilot_name = …` if those
// settable forms aren't already present. The injection lands right before
// `# save configuration` so it's still inside the batch.
//
// Why this is needed: on some Betaflight builds (notably AT32 2025.12.x)
// `diff all` reports the values via header-only and omits the `set` lines.
// Without us supplementing, a backup→restore round-trip silently drops
// craft/pilot.
func injectMissingNameSets(lines []string) []string {
	headerCraft, hasSetCraft := scanNameHeader(lines, "# name:", "set craft_name")
	headerPilot, hasSetPilot := scanNameHeader(lines, "# pilot:", "set pilot_name")
	if (headerCraft == "" || hasSetCraft) && (headerPilot == "" || hasSetPilot) {
		return lines
	}
	// Insert just before the trailing `# save configuration` marker (or
	// at the end if that marker isn't present).
	insertAt := len(lines)
	for i, line := range lines {
		if strings.TrimSpace(line) == "# save configuration" {
			insertAt = i
			break
		}
	}
	var inject []string
	if headerCraft != "" && !hasSetCraft {
		inject = append(inject, "set craft_name = "+headerCraft)
	}
	if headerPilot != "" && !hasSetPilot {
		inject = append(inject, "set pilot_name = "+headerPilot)
	}
	out := make([]string, 0, len(lines)+len(inject))
	out = append(out, lines[:insertAt]...)
	out = append(out, inject...)
	out = append(out, lines[insertAt:]...)
	return out
}

// scanNameHeader returns the value following headerPrefix (e.g. "# name:")
// and whether a matching `set` line (e.g. "set craft_name") already exists.
func scanNameHeader(lines []string, headerPrefix, setPrefix string) (string, bool) {
	var headerVal string
	var hasSet bool
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, headerPrefix) {
			headerVal = strings.TrimSpace(t[len(headerPrefix):])
		}
		if strings.HasPrefix(t, setPrefix+" ") || strings.HasPrefix(t, setPrefix+"=") {
			hasSet = true
		}
	}
	return headerVal, hasSet
}

// isProfileSelector returns true for `profile N` / `rateprofile N` lines —
// these need a longer post-send delay because Betaflight rebuilds derived
// state when switching profiles.
func isProfileSelector(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "profile ") || strings.HasPrefix(t, "rateprofile ")
}

// looksLikeBackup is a sanity check: backup files always contain a
// `batch start` line on its own. Catches the case where someone hands a
// random text file to `bfctl restore`.
func looksLikeBackup(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "batch start" {
			return true
		}
	}
	return false
}

// mentionsRestoreError checks the FC reply for the error markers Betaflight
// emits when a batch line is rejected. The exact strings vary across
// firmware; the substrings here match what current Betaflight prints on
// `Invalid value`, `Unknown`, and the generic `ERROR` markers.
func mentionsRestoreError(reply string) bool {
	low := strings.ToLower(reply)
	for _, needle := range []string{"invalid", "unknown command", "error", "###"} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

// ----- set -----

// cmdSet writes a single CLI `set` line to the FC and (by default) persists
// it via `save`. Anything after the flags is joined with a single space and
// prepended with `set `, so all of these forward identically to the CLI:
//
//	bfctl set pilot_name = Maverick
//	bfctl set pilot_name=Maverick
//	bfctl set craft_name = "My Drone"
//
// On success Betaflight replies "<key> set to <value>" — we surface that to
// stdout. Any other reply is treated as a failure (exit 1) so scripts can
// branch on it.
func cmdSet(args []string) int {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	noSave := fs.Bool("no-save", false, "don't run `save` after the set (change is lost on reboot)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: bfctl set <key> = <value>")
		return exitUsage
	}
	cmd := "set " + strings.Join(fs.Args(), " ")

	path, err := fc.Resolve(*port)
	if err != nil {
		return reportFCErr(err)
	}
	sess, err := fc.Open(path)
	if err != nil {
		return reportFCErr(err)
	}
	defer func() { _ = sess.Close() }()

	reply, err := sess.Run(cmd)
	if err != nil {
		return reportFCErr(err)
	}
	reply = strings.TrimSpace(reply)
	if reply != "" {
		fmt.Println(reply)
	}
	if !strings.Contains(reply, " set to ") {
		fmt.Fprintln(os.Stderr, "bfctl: FC did not confirm the set")
		return exitGeneric
	}

	if *noSave {
		fmt.Fprintln(os.Stderr, "bfctl: --no-save: change will be lost on reboot")
		return exitOK
	}

	if err := sess.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "bfctl: save:", err)
		return exitGeneric
	}
	fmt.Fprintln(os.Stderr, "bfctl: saved (FC is rebooting)")
	return exitOK
}

// ----- msp -----

// cmdMSP runs raw MSP v1 queries against the FC. Four forms:
//
//	bfctl msp                  — scan codes 1..MaxScanCode and print all replies
//	bfctl msp <code>           — query one code by number (decimal or 0x… hex)
//	bfctl msp <name>           — same, but resolve a Betaflight name (e.g. boxnames)
//	bfctl msp <code|name>...   — query each in turn; exit non-zero if any failed
//
// The list form bypasses the denylist (the user named the codes explicitly).
// JSON output for the list form is an array; for the single-code form it
// remains a single object so existing scripts that parse `bfctl msp X --json`
// don't break.
//
// Unlike every other subcommand, msp does NOT enter CLI mode. The FC speaks
// MSP at port-open time; once `#` has been sent, MSP queries no longer work
// until the FC reboots. If queries time out consistently, power-cycle the FC.
func cmdMSP(args []string) int {
	fs := flag.NewFlagSet("msp", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	asJSON := fs.Bool("json", false, "emit JSON")
	timeout := fs.Duration("timeout", 500*time.Millisecond, "per-query timeout")
	from := fs.Uint("from", 1, "scan: lowest code to probe")
	maxCode := fs.Uint("max", uint(msp.SafeScanCeiling), "scan: highest code to probe (cap "+strconv.Itoa(msp.MaxScanCode)+"; codes flagged destructive are skipped)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	path, err := fc.Resolve(*port)
	if err != nil {
		return reportFCErr(err)
	}
	sess, err := fc.OpenMSP(path)
	if err != nil {
		return reportFCErr(err)
	}
	defer func() { _ = sess.Close() }()

	if fs.NArg() == 0 {
		return mspScan(sess, uint8(*from), uint8(*maxCode), *timeout, *asJSON)
	}
	codes := make([]uint8, 0, fs.NArg())
	for i := 0; i < fs.NArg(); i++ {
		c, err := resolveMSPCode(fs.Arg(i))
		if err != nil {
			fmt.Fprintln(os.Stderr, "bfctl:", err)
			return exitUsage
		}
		codes = append(codes, c)
	}
	if len(codes) == 1 {
		return mspQueryOne(sess, codes[0], *timeout, *asJSON)
	}
	return mspQueryList(sess, codes, *timeout, *asJSON)
}

// resolveMSPCode accepts a decimal number, a 0x-prefixed hex number, or an
// MSP name (case-insensitive, with or without the "MSP_" prefix).
func resolveMSPCode(s string) (uint8, error) {
	if n, err := strconv.ParseUint(s, 0, 8); err == nil {
		return uint8(n), nil
	}
	want := strings.ToUpper(s)
	if !strings.HasPrefix(want, "MSP_") {
		want = "MSP_" + want
	}
	for code := uint8(1); code <= msp.MaxScanCode; code++ {
		if strings.EqualFold(msp.Name(code), want) {
			return code, nil
		}
	}
	return 0, fmt.Errorf("unknown MSP code or name: %q", s)
}

// mspResult is the shape of one row in scan output (and one record in JSON).
type mspResult struct {
	Code    int    `json:"code"`
	Name    string `json:"name"`
	Size    int    `json:"size"`
	Hex     string `json:"hex,omitempty"`
	Decoded string `json:"decoded,omitempty"`
}

func makeMSPResult(resp msp.Response) mspResult {
	return mspResult{
		Code:    int(resp.Code),
		Name:    msp.Name(resp.Code),
		Size:    len(resp.Payload),
		Hex:     hex.EncodeToString(resp.Payload),
		Decoded: msp.Decode(resp.Code, resp.Payload),
	}
}

// mspQueryList queries each code in turn and prints the replies. Unlike
// mspScan it does not consult the denylist (the user named the codes
// explicitly) and unlike mspQueryOne it tolerates partial failure: any
// code that times out or is rejected logs to stderr and the loop
// continues, but the exit code is non-zero if any code failed.
func mspQueryList(sess *fc.MSPSession, codes []uint8, timeout time.Duration, asJSON bool) int {
	results := make([]mspResult, 0, len(codes))
	rc := exitOK
	for _, code := range codes {
		resp, err := sess.Query(code, nil, timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bfctl: MSP %d (%s): %v\n", code, msp.Name(code), err)
			rc = exitGeneric
			continue
		}
		if !resp.OK() {
			fmt.Fprintf(os.Stderr, "bfctl: FC rejected MSP %d (%s)\n", code, msp.Name(code))
			rc = exitGeneric
			continue
		}
		results = append(results, makeMSPResult(resp))
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		return rc
	}
	for _, r := range results {
		printMSPResult(r)
	}
	return rc
}

func mspQueryOne(sess *fc.MSPSession, code uint8, timeout time.Duration, asJSON bool) int {
	resp, err := sess.Query(code, nil, timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bfctl:", err)
		return exitGeneric
	}
	if !resp.OK() {
		fmt.Fprintf(os.Stderr, "bfctl: FC rejected MSP %d (%s)\n", code, msp.Name(code))
		return exitGeneric
	}
	r := makeMSPResult(resp)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return exitOK
	}
	printMSPResult(r)
	return exitOK
}

func mspScan(sess *fc.MSPSession, from, max uint8, timeout time.Duration, asJSON bool) int {
	if from < 1 {
		from = 1
	}
	if max > msp.MaxScanCode {
		max = msp.MaxScanCode
	}
	if max < from {
		fmt.Fprintln(os.Stderr, "bfctl: --max < --from")
		return exitUsage
	}
	var results []mspResult
	skipped := 0
	for code := uint16(from); code <= uint16(max); code++ {
		if msp.IsDenylisted(uint8(code)) {
			skipped++
			continue
		}
		resp, err := sess.Query(uint8(code), nil, timeout)
		if err != nil || !resp.OK() {
			// Timeout or FC error response: code isn't supported on this
			// firmware. Skip silently — that's the expected case for most
			// codes, the scan would otherwise be 199 lines of noise.
			continue
		}
		results = append(results, makeMSPResult(resp))
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "bfctl: skipped %d code(s) flagged destructive (use `bfctl msp <code>` to opt in)\n", skipped)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		return exitOK
	}
	for _, r := range results {
		printMSPResult(r)
	}
	fmt.Fprintf(os.Stderr, "bfctl: %d responded in codes %d–%d\n", len(results), from, max)
	return exitOK
}

// printMSPResult emits one human-readable line. Layout:
//
//	NNN MSP_NAME (size B)  decoded text
//	    hex bytes…
//
// The hex line is only printed when there's a payload; the decoded line is
// only printed when Decode() returned something. For codes with neither, the
// header line still goes out so the reader can see the code was supported.
func printMSPResult(r mspResult) {
	fmt.Printf("%3d  %-30s  %d B", r.Code, r.Name, r.Size)
	if r.Decoded != "" {
		fmt.Printf("  %s", r.Decoded)
	}
	fmt.Println()
	if r.Hex != "" {
		fmt.Printf("     %s\n", spacedHex(r.Hex))
	}
}

// spacedHex inserts a space every two hex chars, capped at 32 bytes per
// line so wide payloads don't blow past 80 columns.
func spacedHex(h string) string {
	const bytesPerLine = 32
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			if (i/2)%bytesPerLine == 0 {
				b.WriteString("\n     ")
			} else {
				b.WriteByte(' ')
			}
		}
		end := i + 2
		if end > len(h) {
			end = len(h)
		}
		b.WriteString(h[i:end])
	}
	return b.String()
}

// ----- ports -----

func cmdPorts(args []string) int {
	fs := flag.NewFlagSet("ports", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	ports, err := fc.FindAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bfctl:", err)
		return exitGeneric
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ports)
		return exitOK
	}
	if len(ports) == 0 {
		fmt.Fprintln(os.Stderr, "no Betaflight FCs detected")
		return exitNoFC
	}
	for _, p := range ports {
		fmt.Printf("%s\t%s\t%s:%s\t%s\n", p.Path, p.Product, p.VID, p.PID, p.Serial)
	}
	return exitOK
}

// ----- shared FC plumbing -----

// pullDump opens a session, runs the given Betaflight CLI command (typically
// `diff all` or `dump all`), and returns the body plus the device path used.
func pullDump(explicitPort, cliCmd string) (body, path string, err error) {
	path, err = fc.Resolve(explicitPort)
	if err != nil {
		return "", "", err
	}
	sess, err := fc.Open(path)
	if err != nil {
		return "", path, err
	}
	defer func() { _ = sess.Close() }()
	body, err = sess.Run(cliCmd)
	if err != nil {
		return "", path, err
	}
	return body, path, nil
}

// reportFCErr prints err to stderr and returns the matching exit code.
func reportFCErr(err error) int {
	switch {
	case errors.Is(err, fc.ErrNoFC):
		fmt.Fprintln(os.Stderr, "bfctl: no Betaflight FC found (is it plugged in? is Chrome holding the port?)")
		return exitNoFC
	}
	var multi *fc.ErrMultipleFCs
	if errors.As(err, &multi) {
		fmt.Fprintln(os.Stderr, "bfctl:", multi.Error())
		fmt.Fprintln(os.Stderr, "       use --port to pick one")
		return exitMultipleFCs
	}
	msg := err.Error()
	if strings.Contains(msg, "resource busy") || strings.Contains(msg, "Resource busy") || strings.Contains(strings.ToLower(msg), "in use") {
		fmt.Fprintln(os.Stderr, "bfctl: port already in use (quit Chrome / close Configurator and retry)")
		return exitPortInUse
	}
	fmt.Fprintln(os.Stderr, "bfctl:", err)
	return exitGeneric
}
