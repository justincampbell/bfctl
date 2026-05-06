// bfctl is a CLI for talking to a Betaflight flight controller over USB.
//
// stdout is data; stderr is for humans (progress, hints, errors). All
// commands accept --port to override auto-detection. Exit codes are
// stable so other tools (and agents) can branch on them.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
  dump     Print full configuration (`+"`diff all`"+`) to stdout
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

	body, _, err := pullDump(*port)
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

// ----- dump -----

func cmdDump(args []string) int {
	fs := flag.NewFlagSet("dump", flag.ContinueOnError)
	port := fs.String("port", "", "serial device path (default: auto-detect)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	body, _, err := pullDump(*port)
	if err != nil {
		return reportFCErr(err)
	}
	out := formatBackup(body)
	fmt.Print(out)
	if !strings.HasSuffix(out, "\n") {
		fmt.Println()
	}
	return exitOK
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

	body, _, err := pullDump(*port)
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

	body, path, err := pullDump(*port)
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

// pullDump opens a session, runs `diff all`, and returns the body and the
// device path used.
func pullDump(explicitPort string) (body, path string, err error) {
	path, err = fc.Resolve(explicitPort)
	if err != nil {
		return "", "", err
	}
	sess, err := fc.Open(path)
	if err != nil {
		return "", path, err
	}
	defer func() { _ = sess.Close() }()
	body, err = sess.Run("diff all")
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
