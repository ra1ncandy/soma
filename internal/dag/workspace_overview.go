package dag

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
)

const (
	overviewMaxEntries = 120
	overviewMaxChars   = 4000
)

// workspaceOverview lists the workspace's files (path and size) for the first turn of a run, so
// the model can go straight to reading what matters instead of spending a call on fs_list. It is
// bounded: large trees are cut off with a pointer to fs_list and fs_grep. Returns "" when there is
// nothing useful to show.
func workspaceOverview(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	var lines []string
	chars, more := 0, 0
	_ = fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == ".soma" || heavyDir(name) {
				return fs.SkipDir
			}
			return nil
		}
		if name == ".soma" || !d.Type().IsRegular() {
			return nil
		}
		if len(lines) >= overviewMaxEntries || chars >= overviewMaxChars {
			more++
			return nil
		}
		line := p
		if info, iErr := d.Info(); iErr == nil {
			line = fmt.Sprintf("%s (%s)", p, humanSize(info.Size()))
		}
		lines = append(lines, line)
		chars += len(line) + 1
		return nil
	})
	if len(lines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Workspace files at the start of this run (no need to call fs_list for these):\n")
	b.WriteString(strings.Join(lines, "\n"))
	if more > 0 {
		fmt.Fprintf(&b, "\n... and %d more files not shown; use fs_list or fs_grep to find them.", more)
	}
	return b.String()
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
