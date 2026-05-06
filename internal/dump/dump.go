// Package dump parses small bits of metadata out of a Betaflight `diff all`
// body. It does not attempt to understand the full grammar — only the fields
// needed to name backup files and answer `bfctl info`.
package dump

import (
	"bufio"
	"strings"
)

// Info is the parsed metadata view of a dump.
type Info struct {
	Board     string
	MCUID     string
	Firmware  string
	CraftName string
	PilotName string
}

// Parse extracts metadata from a dump body.
func Parse(body string) Info {
	var info Info
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "board_name "):
			info.Board = strings.TrimSpace(strings.TrimPrefix(line, "board_name"))
		case strings.HasPrefix(line, "mcu_id "):
			info.MCUID = strings.TrimSpace(strings.TrimPrefix(line, "mcu_id"))
		case strings.HasPrefix(line, "# Betaflight"):
			info.Firmware = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		case strings.HasPrefix(line, "set craft_name"):
			info.CraftName = setValue(line)
		case strings.HasPrefix(line, "set pilot_name"):
			info.PilotName = setValue(line)
		}
	}
	return info
}

// Get returns the right-hand side of a `set <key> = <value>` line in the
// dump, or "" if the key is missing.
func Get(body, key string) (string, bool) {
	prefix := "set " + key
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		// Guard against prefix collisions like `set craft_name` vs
		// `set craft_name_pos`.
		rest := line[len(prefix):]
		if rest != "" && rest[0] != ' ' && rest[0] != '=' {
			continue
		}
		return setValue(line), true
	}
	return "", false
}

// setValue returns the value to the right of "=" on a `set k = v` line.
func setValue(line string) string {
	i := strings.Index(line, "=")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(line[i+1:])
}
