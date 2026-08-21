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
