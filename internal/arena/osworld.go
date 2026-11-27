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
// files and checker functions, but no desktop VM. An agent edits the files in its workspace with
// code and command-line tools instead of driving a GUI; the workspace stands in for the task's
// home directory (/home/user). Only tasks whose checker reads files are included, and only those
// where the checker was shown to fail on the starting files and to be passable (see
// osworld/prepare.py). LibreOffice is deliberately not involved: its task domains are left out and
// nothing here runs it. Scores are not comparable with the OSWorld leaderboard.

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

var osworldShort = map[string]string{"gimp": "image", "vs_code": "vscode", "multi_apps": "multi"}

// OSWorldTasks loads the validated task manifest under root (a directory prepared with
// osworld/prepare.py: src/ is the OSWorld checkout, venv/ runs the checkers).
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
			// The task's home directory is the workspace, for the contestant's shell too.
			HomeIsWorkspace: true,
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
	b.WriteString("\n\nThis is a file-only version of the task: there is no desktop, GUI or application to open. " +
		"The current directory is the user's home directory (~, written /home/user in the task); work only inside it. ")
	if len(files) > 0 {
		fmt.Fprintf(&b, "Starting file(s): %s. ", strings.Join(files, ", "))
	}
	fmt.Fprintf(&b, "The result is checked in this file, relative to the home directory: %s. ", strings.Join(e.Results, ", "))
	b.WriteString("Produce it with code or command-line tools (Python 3 with Pillow, ImageMagick and ffmpeg are installed).")
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

// gradeOSWorld runs OSWorld's checker on the files in dir.
func gradeOSWorld(ctx context.Context, root, python string, e osworldEntry, dir string) (bool, string) {
	f, err := os.CreateTemp("", "arena-osworld-grade-*.py")
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, _ = f.WriteString(osworldGrader)
	_ = f.Close()

	gctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(gctx, python, "-I", f.Name(),
		"--src", filepath.Join(root, "src"), "--task", filepath.Join(root, e.TaskFile),
		"--workspace", dir, "--cache", filepath.Join(root, e.Cache))
	out, err := cmd.Output()
	verdict := strings.TrimSpace(string(out))
	if i := strings.LastIndex(verdict, "\n"); i >= 0 {
		verdict = verdict[i+1:]
	}
	if err == nil {
		return true, verdict + " PASS"
	}
	return false, verdict
}
