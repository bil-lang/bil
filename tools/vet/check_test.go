package vet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckHonorsLineDirective pins the other half of bilc's .bil-source
// line-mapping (see bilc.go's resync): Check itself needs no line-tracking
// of its own, because go/parser applies a `//line` directive embedded in
// the file unconditionally, before Check ever sees a token.Pos. This
// fixture's leading `//line original.bil:99` (see
// testdata/violation_with_line_directive.go) is exactly what bilc now
// emits ahead of any verbatim source it copies into the transpiled Go it
// hands to Check — if this test ever fails, either go/parser's directive
// handling has changed, or Check has stopped routing every message through
// fset.Position.
func TestCheckHonorsLineDirective(t *testing.T) {
	messages, err := Check("testdata/violation_with_line_directive.go")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("got %d messages, want 1: %v", len(messages), messages)
	}
	const wantPrefix = "/virtual/original.bil:99:"
	if !strings.HasPrefix(messages[0], wantPrefix) {
		t.Errorf("message = %q, want prefix %q", messages[0], wantPrefix)
	}
}

// TestCheckResolvesExternalModule proves the module-aware resolution
// mechanism Check's extraGoMod param exists for (real production use:
// tools/bil/main.go's runCmd passing a `replace emulator => ...` line for
// a placed/link program) actually works, without depending on the real
// sibling emulator repo existing at a fixed relative path -- a small,
// synthetic local module stands in for it here, built fresh in a t.TempDir
// so its replace target is a real, valid absolute path every run.
//
// Before the go/importer -> go/packages switch this fixture's import
// could never have resolved at all (go/importer's "source" mode has no
// concept of go.mod/replace); this test would have failed with a
// "could not import" typeErr, exactly the failure mode the real
// emulator/bilink import used to hit.
func TestCheckResolvesExternalModule(t *testing.T) {
	modDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module fakepkg\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "lib.go"), []byte("package fakepkg\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := "package main\n\nimport \"fakepkg\"\n\nfunc main() {\n\tprintln(fakepkg.Hello())\n}\n"
	fixture := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(fixture, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	messages, err := Check(fixture, "require fakepkg v0.0.0", "replace fakepkg => "+modDir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("got %d messages, want 0 (a valid program importing a real, replace-resolved external module should check clean): %v", len(messages), messages)
	}
}
