// bfctl is a CLI for talking to a Betaflight flight controller over USB.
//
// stdout is data; stderr is for humans (progress, hints, errors). All
// commands accept --port to override auto-detection. Exit codes are
// stable so other tools (and agents) can branch on them.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peterh/liner"

	"github.com/justincampbell/bfctl/internal/dump"
	"github.com/justincampbell/bfctl/internal/fc"
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
	case "diff":
		os.Exit(cmdDiff(os.Args[2:]))
	case "dump":
		os.Exit(cmdDump(os.Args[2:]))
	case "exec":
		os.Exit(cmdExec(os.Args[2:]))
	case "get":
		os.Exit(cmdGet(os.Args[2:]))
	case "info":
		os.Exit(cmdInfo(os.Args[2:]))
	case "ports":
		os.Exit(cmdPorts(os.Args[2:]))
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
  diff     Print non-default settings (`+"`diff all`"+`)
  dump     Print every setting (`+"`dump all`"+`, including defaults)
  exec     Send one CLI command verbatim and print the reply
  get      Print one setting's value
  info     Print FC metadata (board, firmware, craft, …)
  ports    List detected Betaflight FCs
  set      Write a setting and persist it (reboots the FC)
  version  Print version

Run 'bfctl <command> --help' for command-specific flags.
`)
}

// ----- backup -----

func cmdBackup(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	out := fs.String("out", ".", "output directory")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	body, _, err := pullDump(*port, "diff all")
	if err != nil {
		return reportFCErr(err)
	}

	info := dump.Parse(body)
	name := backupFilename(info, time.Now())
	path := filepath.Join(*out, name)

	if err := os.WriteFile(path, []byte(formatBackup(body)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "bfctl:", err)
		return exitGeneric
	}
	fmt.Println(path)
	return exitOK
}

// formatBackup wraps a raw `diff all` body into the file format the web
// Configurator produces: a leading `defaults nosave` so the file is a
// self-contained restore script, two blank separator lines, then the body.
// The FC's `diff all` output already starts with a newline before
// "# version", so prepending "defaults nosave\n\n" yields three newlines
// (two blank lines) between the prefix and the body.
func formatBackup(body string) string {
	if !strings.HasPrefix(body, "\n") {
		body = "\n" + body
	}
	return "defaults nosave\n\n" + body
}

// backupFilename matches the convention used by app.betaflight.com so saved
// files interleave naturally:
//
//	BTFL_cli_backup_<CRAFT>_<YYYYMMDD>_<HHMMSS>_<BOARD>.txt
func backupFilename(info dump.Info, when time.Time) string {
	craft := sanitize(info.CraftName)
	if craft == "" {
		craft = "UNKNOWN"
	}
	board := sanitize(info.Board)
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

func cmdInfo(args []string) int {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	body, path, err := pullDump(*port, "diff all")
	if err != nil {
		return reportFCErr(err)
	}

	info := dump.Parse(body)

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
	fmt.Printf("Pilot:      %s\n", info.PilotName)
	return exitOK
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
