package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryStatusCreatesCanonicalLedger(t *testing.T) {
	var output bytes.Buffer
	path := filepath.Join(t.TempDir(), "state.sqlite")
	if err := memoryStatus([]string{"--database", path}, &output); err != nil {
		t.Fatalf("memory status: %v", err)
	}
	if got := output.String(); !strings.Contains(got, "0 active, 0 deleted") {
		t.Fatalf("status output=%q", got)
	}
}

func TestMemoryCLIHasNoImportOrExportSurface(t *testing.T) {
	for _, command := range []string{"import", "export"} {
		if err := runMemoryCommand([]string{command}); err == nil {
			t.Fatalf("memory %s unexpectedly supported", command)
		}
	}
}

func TestMemoryMaintenanceStatusAndCandidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	var status bytes.Buffer
	if err := memoryMaintenance([]string{"status", "--database", path}, &status); err != nil {
		t.Fatal(err)
	}
	if got := status.String(); !strings.Contains(got, "history ID 0") || !strings.Contains(got, "Latest run: none") {
		t.Fatalf("maintenance status output=%q", got)
	}
	if err := memoryMaintenance([]string{"candidates", "--database", path, "--run-id", "0"}, &bytes.Buffer{}); err == nil {
		t.Fatal("non-positive maintenance run ID was accepted")
	}
}
