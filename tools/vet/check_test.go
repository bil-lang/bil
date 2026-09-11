package vet

import (
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
