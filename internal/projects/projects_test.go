package projects

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestRegisterAndList(t *testing.T) {
	// Create a temporary directory for the test
	tmpDir := t.TempDir()

	// Override the projects file path for testing
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Test registering a project
	err := Register("/home/user/project1", "/home/user/project1/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// List projects
	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(projects))
	}

	if projects[0].Path != "/home/user/project1" {
		t.Errorf("Expected path /home/user/project1, got %s", projects[0].Path)
	}

	if projects[0].DataDir != "/home/user/project1/.crush" {
		t.Errorf("Expected data_dir /home/user/project1/.crush, got %s", projects[0].DataDir)
	}

	// Register another project
	err = Register("/home/user/project2", "/home/user/project2/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, err = List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 2 {
		t.Fatalf("Expected 2 projects, got %d", len(projects))
	}

	// Most recent should be first
	if projects[0].Path != "/home/user/project2" {
		t.Errorf("Expected most recent project first, got %s", projects[0].Path)
	}
}

func TestRegisterUpdatesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Register a project
	err := Register("/home/user/project1", "/home/user/project1/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, _ := List()
	firstAccess := projects[0].LastAccessed

	// Wait a bit and re-register
	time.Sleep(10 * time.Millisecond)

	err = Register("/home/user/project1", "/home/user/project1/.crush-new")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, _ = List()

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project after update, got %d", len(projects))
	}

	if projects[0].DataDir != "/home/user/project1/.crush-new" {
		t.Errorf("Expected updated data_dir, got %s", projects[0].DataDir)
	}

	if !projects[0].LastAccessed.After(firstAccess) {
		t.Error("Expected LastAccessed to be updated")
	}
}

func TestLoadEmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// List before any projects exist
	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 0 {
		t.Errorf("Expected 0 projects, got %d", len(projects))
	}
}

func TestProjectsFilePath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	expected := filepath.Join(tmpDir, "crush", "projects.json")
	actual := projectsFilePath()

	if actual != expected {
		t.Errorf("Expected %s, got %s", expected, actual)
	}
}

func TestRegisterWithParentDataDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Register a project where .crush is in a parent directory.
	// e.g., working in /home/user/monorepo/packages/app but .crush is at /home/user/monorepo/.crush
	err := Register("/home/user/monorepo/packages/app", "/home/user/monorepo/.crush")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(projects))
	}

	if projects[0].Path != "/home/user/monorepo/packages/app" {
		t.Errorf("Expected path /home/user/monorepo/packages/app, got %s", projects[0].Path)
	}

	if projects[0].DataDir != "/home/user/monorepo/.crush" {
		t.Errorf("Expected data_dir /home/user/monorepo/.crush, got %s", projects[0].DataDir)
	}
}

func TestRegisterWithExternalDataDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Register a project where .crush is in a completely different location.
	// e.g., project at /home/user/project but data stored at /var/data/crush/myproject
	err := Register("/home/user/project", "/var/data/crush/myproject")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("Expected 1 project, got %d", len(projects))
	}

	if projects[0].Path != "/home/user/project" {
		t.Errorf("Expected path /home/user/project, got %s", projects[0].Path)
	}

	if projects[0].DataDir != "/var/data/crush/myproject" {
		t.Errorf("Expected data_dir /var/data/crush/myproject, got %s", projects[0].DataDir)
	}
}

func TestRegisterOrdersTiedTimestamps(t *testing.T) {
	// Two registrations that land in the same clock tick must still order
	// most-recent-first. This is the CI flake seen only on windows-latest,
	// where time.Now() granularity is coarse enough to tie routinely; pinning
	// the clock reproduces it on every platform.
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	fixed := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	orig := timeNow
	timeNow = func() time.Time { return fixed }
	t.Cleanup(func() { timeNow = orig })

	for _, p := range []string{"/p1", "/p2", "/p3"} {
		if err := Register(p, p+"/.crush"); err != nil {
			t.Fatalf("Register(%s) failed: %v", p, err)
		}
	}

	projects, err := List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	got := make([]string, len(projects))
	for i, p := range projects {
		got[i] = p.Path
	}
	want := []string{"/p3", "/p2", "/p1"}
	if !slices.Equal(got, want) {
		t.Errorf("tied timestamps ordered %v, want %v", got, want)
	}

	// Re-registering an existing project must move it back to the front.
	if err := Register("/p1", "/p1/.crush"); err != nil {
		t.Fatalf("re-Register failed: %v", err)
	}
	projects, err = List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if projects[0].Path != "/p1" {
		t.Errorf("re-registered project not first: got %s", projects[0].Path)
	}
	if len(projects) != 3 {
		t.Errorf("re-register duplicated an entry: got %d projects", len(projects))
	}
}

func TestRegisterCapsAtMaxEntries(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

	// Build list with MaxEntries+10 projects via Save (not Register).
	list := &ProjectList{}
	for i := range MaxEntries + 10 {
		list.Projects = append(list.Projects, Project{
			Path:         fmt.Sprintf("/p%d", i),
			DataDir:      fmt.Sprintf("/p%d/.crush", i),
			LastAccessed: time.Now().UTC(),
		})
	}
	if err := Save(list); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Register one new project.
	newPath := "/new-project"
	if err := Register(newPath, newPath+"/.crush"); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if n := len(loaded.Projects); n > MaxEntries {
		t.Errorf("got %d projects after Register, want <= %d", n, MaxEntries)
	}

	found := false
	for _, p := range loaded.Projects {
		if p.Path == newPath {
			found = true
			break
		}
	}
	if !found {
		t.Error("registered path not found in project list")
	}
}

func BenchmarkRegisterCap(b *testing.B) {
	for _, n := range []int{0, 100, 500, 1000, MaxEntries, 2500} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			tmpDir := b.TempDir()
			b.Setenv("XDG_DATA_HOME", tmpDir)
			b.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(tmpDir, "crush"))

			// Pre-seed via Save, never loop Register for seeding.
			list := &ProjectList{}
			for i := range n {
				list.Projects = append(list.Projects, Project{
					Path:         fmt.Sprintf("/p%d", i),
					DataDir:      fmt.Sprintf("/p%d/.crush", i),
					LastAccessed: time.Now().UTC(),
				})
			}
			if err := Save(list); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			for b.Loop() {
				_ = Register("/new-path-bench", "/new-path-bench/.crush")
			}
		})
	}
}
