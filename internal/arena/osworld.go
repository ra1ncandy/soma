package arena

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The "osworld" tier replicates OSWorld tasks on plain files: the same instructions, starting
// files and checker functions, but no desktop VM. An agent edits the document in its workspace
// with whatever tools it has (Python libraries, headless LibreOffice) instead of driving a GUI.
// Only tasks whose checker reads files are included, and only those where the checker was shown
// to fail on the starting files and pass on OSWorld's gold file (see osworld/prepare.py).
// Scores are therefore not comparable with the OSWorld leaderboard.

//go:embed osworld/grade.py
var osworldGrader string

// TaskTools is tooling a task needs that a contestant may not have on its default path.
type TaskTools struct {
	// PythonPath is a host directory of Python packages to put on PYTHONPATH.
	PythonPath string
	// ROPaths are host paths a sandboxed contestant must be able to read (application config).
	ROPaths []string
}

type osworldEntry struct {
	ID          string `json:"id"`
	Domain      string `json:"domain"`
	Instruction string `json:"instruction"`
	TaskFile    string `json:"task_file"`
	Cache       string `json:"cache"`
	Inputs      []struct {
		Src  string `json:"src"`
		Dest string `json:"dest"`
	} `json:"inputs"`
	Results []string `json:"results"`
}

var osworldShort = map[string]string{
	"libreoffice_calc": "calc", "libreoffice_writer": "writer", "libreoffice_impress": "impress", "gimp": "gimp",
}

// OSWorldTasks loads the validated task manifest under root (a directory prepared with
// osworld/prepare.py: src/ is the OSWorld checkout, venv/ runs the checkers, pylib/ holds the
// Python libraries offered to contestants).
func OSWorldTasks(root string) ([]Task, error) {
	raw, err := os.ReadFile(filepath.Join(root, "tasks.json"))
	if err != nil {
		return nil, fmt.Errorf("osworld tier: %w (prepare %s with internal/arena/osworld/prepare.py)", err, root)
	}
	var entries []osworldEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("osworld tier: %s: %w", filepath.Join(root, "tasks.json"), err)
	}
	python := filepath.Join(root, "venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		return nil, fmt.Errorf("osworld tier: checker environment missing: %w", err)
	}
	tools := &TaskTools{PythonPath: filepath.Join(root, "pylib")}
	if _, err := os.Stat("/etc/libreoffice"); err == nil {
		tools.ROPaths = append(tools.ROPaths, "/etc/libreoffice")
	}
	var out []Task
	for _, e := range entries {
		e := e
		short := osworldShort[e.Domain]
		if short == "" {
			short = e.Domain
		}
		id := e.ID
		if len(id) > 8 {
			id = id[:8]
		}
		out = append(out, Task{
			ID:       "osw-" + short + "-" + id,
			Tier:     "osworld",
			MaxCalls: 30,
			Goal:     osworldGoal(e),
			Tools:    tools,
			Generate: func(dir string) error {
				for _, in := range e.Inputs {
					if err := copyFile(filepath.Join(root, in.Src), filepath.Join(dir, in.Dest)); err != nil {
						return err
					}
				}
				return nil
			},
			GradeFunc: func(ctx context.Context, dir string) (bool, string) {
				return gradeOSWorld(ctx, root, python, e, dir)
			},
		})
	}
	return out, nil
}

func osworldGoal(e osworldEntry) string {
	var files []string
	for _, in := range e.Inputs {
		files = append(files, in.Dest)
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(e.Instruction))
	b.WriteString("\n\nThis is a file-only version of the task: there is no desktop or GUI. ")
	if len(files) > 0 {
		fmt.Fprintf(&b, "The file(s) are in the current directory: %s. ", strings.Join(files, ", "))
	}
	fmt.Fprintf(&b, "The result is checked in: %s (same name and format). ", strings.Join(e.Results, ", "))
	b.WriteString("Make the change with code, for example Python (openpyxl, python-docx, python-pptx and Pillow are installed) " +
		"or LibreOffice in headless mode (soffice --headless). Leave no other changes in those files.")
	return b.String()
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// gradeOSWorld runs OSWorld's checker on the files in dir. A spreadsheet that fails as written is
// checked once more after a LibreOffice re-save, which is what OSWorld's own post-step does
// (Ctrl+S in the GUI): it fills in the cached values of formulas written by a library.
func gradeOSWorld(ctx context.Context, root, python string, e osworldEntry, dir string) (bool, string) {
	f, err := os.CreateTemp("", "arena-osworld-grade-*.py")
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, _ = f.WriteString(osworldGrader)
	_ = f.Close()

	run := func(workspace string) (bool, string) {
		gctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(gctx, python, "-I", f.Name(),
			"--src", filepath.Join(root, "src"), "--task", filepath.Join(root, e.TaskFile),
			"--workspace", workspace, "--cache", filepath.Join(root, e.Cache))
		out, err := cmd.Output()
		verdict := strings.TrimSpace(string(out))
		if i := strings.LastIndex(verdict, "\n"); i >= 0 {
			verdict = verdict[i+1:]
		}
		return err == nil, verdict
	}
	ok, verdict := run(dir)
	if ok {
		return true, verdict + " PASS"
	}
	resaved, cleanup, changed := resaveSpreadsheets(ctx, dir, e.Results)
	defer cleanup()
	if changed {
		if ok2, verdict2 := run(resaved); ok2 {
			return true, verdict2 + " (after LibreOffice re-save) PASS"
		}
	}
	return false, verdict
}

// resaveSpreadsheets copies the result files into a scratch directory and re-saves the .xlsx
// ones with headless LibreOffice. It reports whether anything was re-saved.
func resaveSpreadsheets(ctx context.Context, dir string, results []string) (string, func(), bool) {
	soffice, err := exec.LookPath("soffice")
	if err != nil {
		return "", func() {}, false
	}
	scratch, err := os.MkdirTemp("", "arena-osworld-resave-")
	if err != nil {
		return "", func() {}, false
	}
	cleanup := func() { _ = os.RemoveAll(scratch) }
	changed := false
	for _, rel := range results {
		src := filepath.Join(dir, rel)
		dst := filepath.Join(scratch, rel)
		if copyFile(src, dst) != nil {
			continue
		}
		if !strings.EqualFold(filepath.Ext(rel), ".xlsx") {
			continue
		}
		outDir, err := os.MkdirTemp("", "arena-osworld-lo-")
		if err != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		cmd := exec.CommandContext(cctx, soffice, "--headless", "--convert-to", "xlsx", "--outdir", outDir, src)
		// A private profile: never touch the operator's LibreOffice settings or a running instance.
		cmd.Env = append(os.Environ(), "HOME="+outDir)
		runErr := cmd.Run()
		cancel()
		if runErr == nil && copyFile(filepath.Join(outDir, filepath.Base(rel)), dst) == nil {
			changed = true
		}
		_ = os.RemoveAll(outDir)
	}
	return scratch, cleanup, changed
}
