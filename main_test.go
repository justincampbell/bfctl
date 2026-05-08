package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justincampbell/bfctl/internal/dump"
)

func TestResolveBackupPath(t *testing.T) {
	tmp := t.TempDir()
	when := time.Date(2026, 5, 8, 12, 34, 56, 0, time.UTC)
	info := dump.Info{Board: "TESTBOARD", CraftName: "TestCraft"}
	autoName := backupFilename(info, when)

	cases := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "existing directory uses generated filename",
			out:  tmp,
			want: filepath.Join(tmp, autoName),
		},
		{
			name: "non-existent file path is used verbatim",
			out:  filepath.Join(tmp, "custom.txt"),
			want: filepath.Join(tmp, "custom.txt"),
		},
		{
			name: "nested non-existent path is used verbatim",
			out:  filepath.Join(tmp, "subdir", "nested.txt"),
			want: filepath.Join(tmp, "subdir", "nested.txt"),
		},
	}
	for _, c := range cases {
		got := resolveBackupPath(c.out, info, when)
		if got != c.want {
			t.Errorf("%s: resolveBackupPath(%q) = %q, want %q", c.name, c.out, got, c.want)
		}
	}
}

func TestBackupFilenameUppercaseCraft(t *testing.T) {
	when := time.Date(2026, 5, 8, 12, 34, 56, 0, time.UTC)
	info := dump.Info{Board: "TESTBOARD", CraftName: "LionBee3"}
	got := backupFilename(info, when)
	if !strings.Contains(got, "LIONBEE3") {
		t.Errorf("backupFilename() = %q, want craft uppercased to LIONBEE3", got)
	}
}
