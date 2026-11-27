package arena

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOSWorldGoalSaysItIsFileOnly(t *testing.T) {
	var e osworldEntry
	e.Instruction = "Make the header bold. "
	e.Inputs = append(e.Inputs, struct {
		Src  string `json:"src"`
		Dest string `json:"dest"`
	}{Src: "cache/x/in.xlsx", Dest: "Book.xlsx"})
	e.Results = []string{"Book.xlsx"}
	goal := osworldGoal(e)
	for _, want := range []string{"Make the header bold.", "no desktop or GUI", "Book.xlsx", "soffice --headless"} {
		if !strings.Contains(goal, want) {
			t.Errorf("goal lacks %q:\n%s", want, goal)
		}
	}
}

func TestOSWorldTasksNeedsAPreparedDirectory(t *testing.T) {
	if _, err := OSWorldTasks(t.TempDir()); err == nil || !strings.Contains(err.Error(), "prepare.py") {
		t.Fatalf("an unprepared directory must fail with a pointer to prepare.py, got %v", err)
	}
}

// With a prepared directory present, every loaded task must behave like the arena's other tasks:
// its checker rejects the starting files. (That it accepts OSWorld's gold file is established by
// prepare.py when the manifest is built.)
func TestOSWorldSeedFailsLive(t *testing.T) {
	home, _ := os.UserHomeDir()
	root := os.Getenv("SOMA_ARENA_OSWORLD_DIR")
	if root == "" {
		root = filepath.Join(home, ".local", "share", "soma-arena", "osworld")
	}
	tasks, err := OSWorldTasks(root)
	if err != nil {
		t.Skipf("no prepared OSWorld directory: %v", err)
	}
	if testing.Short() {
		tasks = tasks[:1]
	} else if len(tasks) > 4 {
		tasks = []Task{tasks[0], tasks[len(tasks)/3], tasks[2*len(tasks)/3], tasks[len(tasks)-1]}
	}
	for _, task := range tasks {
		dir := t.TempDir()
		if err := task.Seed(dir); err != nil {
			t.Fatalf("%s: seed: %v", task.ID, err)
		}
		if task.Tier != "osworld" || task.Tools == nil || !strings.HasPrefix(task.ID, "osw-") {
			t.Fatalf("%s: unexpected task shape %+v", task.ID, task)
		}
		if ok, out := grade(context.Background(), task, dir); ok {
			t.Errorf("%s: checker accepted the untouched starting files (%s)", task.ID, out)
		}
	}
}
