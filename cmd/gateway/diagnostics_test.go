package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiagnosticsPrune(t *testing.T) {
	d := loadDiagnostics(t.TempDir())
	for i := 0; i < maxDiagnostics+5; i++ {
		if err := d.save(diagnosticReport{Kind: "crash", Build: "37", Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(filepath.Join(d.dir, "*.json"))
	if len(files) != maxDiagnostics {
		t.Fatalf("kept %d reports", len(files))
	}
	if _, err := os.Stat(d.dir); err != nil {
		t.Fatal(err)
	}
}
