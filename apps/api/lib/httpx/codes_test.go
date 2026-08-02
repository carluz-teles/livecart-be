package httpx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"testing"
)

var upperSnake = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)*$`)

// catalogValues parses codes.go and returns every string value declared with the
// typed `Code`. Reading the source (rather than a hand-kept table) means the test
// covers the WHOLE catalog automatically — a new code can never slip in unchecked.
func catalogValues(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "codes.go", nil, 0)
	if err != nil {
		t.Fatalf("parse codes.go: %v", err)
	}

	var values []string
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		id, ok := spec.Type.(*ast.Ident)
		if !ok || id.Name != "Code" {
			return true
		}
		for _, v := range spec.Values {
			lit, ok := v.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", lit.Value, err)
			}
			values = append(values, s)
		}
		return true
	})
	return values
}

// TestCodes_UpperSnakeAndUnique — AC 6: every catalog Code is UPPER_SNAKE and no
// value is duplicated. Sanity: the sentinel INTERNAL/VALIDATION_FAILED and the
// owner's headline CART_EXPIRED are present.
func TestCodes_UpperSnakeAndUnique(t *testing.T) {
	values := catalogValues(t)
	if len(values) < 10 {
		t.Fatalf("catalog looks empty (%d codes) — parser found nothing", len(values))
	}

	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if !upperSnake.MatchString(v) {
			t.Errorf("code %q is not UPPER_SNAKE_CASE", v)
		}
		if seen[v] {
			t.Errorf("duplicate code %q", v)
		}
		seen[v] = true
	}

	for _, must := range []string{"INTERNAL", "VALIDATION_FAILED", "CART_EXPIRED"} {
		if !seen[must] {
			t.Errorf("catalog is missing expected code %q", must)
		}
	}
}

// TestCode_String — the typed Code renders to its underlying string (this is what
// travels on the wire as Envelope.reason via DomainError/WithCode).
func TestCode_String(t *testing.T) {
	if CodeInternal.String() != "INTERNAL" {
		t.Fatalf("Code.String(): want INTERNAL, got %q", CodeInternal.String())
	}
}
