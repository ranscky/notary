package record

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVocabularyMatchesItsDeclarations is the gate that makes the exported
// vocabulary complete for the declaration forms it can read.
//
// It source-scans this package's non-test .go files and resolves every
// declaration typed EventType or ReasonKind in these forms:
//
//	ReasonFoo ReasonKind = "foo"             // explicit type + string literal
//	ReasonFoo ReasonKind = ReasonKind("foo") // explicit type + conversion
//	ReasonFoo = ReasonKind("foo")            // conversion carries the type
//	var ReasonFoo ReasonKind = "foo"         // the var form
//
// Each resolved value must appear in EventTypes() / ReasonKinds(), and neither
// accessor may list a value no declaration produces. A declaration typed as the
// vocabulary whose value the scanner cannot read statically (a computed value,
// or a spec that repeats the previous one) fails too, so it cannot slip past
// silently.
//
// WHAT IT DOES NOT SEE. An untyped `const ReasonFoo = "foo"` is a plain string
// constant: assignable to ReasonKind, but not one, so it carries no type to key
// on and is deliberately not resolved. Keying on a name prefix instead would
// flag unrelated string constants, and an honest limit is the better trade.
// Such a value reaching a producer is caught at render -- Render fails loudly
// rather than emitting an unphrased line -- not here.
func TestVocabularyMatchesItsDeclarations(t *testing.T) {
	declared, unreadable := scanTypedStringConstants(t, "EventType", "ReasonKind")

	assert.Emptyf(t, unreadable,
		"the gate cannot statically read %d EventType/ReasonKind declaration(s): %v",
		len(unreadable), unreadable)

	assertVocabulary(t, "EventType", declared["EventType"], eventTypeStrings(EventTypes()))
	assertVocabulary(t, "ReasonKind", declared["ReasonKind"], reasonKindStrings(ReasonKinds()))
}

// assertVocabulary fails when a declared constant is missing from the exported
// list, or when the list carries a value no constant declares.
func assertVocabulary(t *testing.T, typeName string, declared map[string]string, listed []string) {
	t.Helper()

	listedValues := make(map[string]bool, len(listed))
	for _, v := range listed {
		listedValues[v] = true
	}
	for name, value := range declared {
		assert.Truef(t, listedValues[value],
			"%s constant %s = %q is missing from %s()", typeName, name, value, typeName+"s")
	}

	declaredByValue := make(map[string]string, len(declared))
	for name, value := range declared {
		declaredByValue[value] = name
	}
	for _, v := range listed {
		_, ok := declaredByValue[v]
		assert.Truef(t, ok,
			"%s() lists %q, which no %s constant declares", typeName+"s", v, typeName)
	}
}

// scanTypedStringConstants parses every non-test .go file in the CURRENT
// package directory -- the test runs with that directory as its working
// directory -- and returns, per type name, the map from declaration name to the
// string value it is declared with, plus the names of declarations typed as
// that vocabulary whose value it could not read statically.
func scanTypedStringConstants(t *testing.T, typeNames ...string) (declared map[string]map[string]string, unreadable []string) {
	t.Helper()

	wanted := make(map[string]bool, len(typeNames))
	declared = make(map[string]map[string]string, len(typeNames))
	for _, name := range typeNames {
		wanted[name] = true
		declared[name] = map[string]string{}
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		name := fi.Name()
		return !fi.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	require.NoError(t, err, "the test must run with its package directory as the working directory")
	require.NotEmpty(t, pkgs, "parsed no package from the record package directory")

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				gd, ok := n.(*ast.GenDecl)
				if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
					return true
				}
				// Within a const group a spec that omits both type and
				// expression repeats the previous spec's type, so carry it
				// forward to the next spec.
				lastType := ""
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					specType, _, _ := classifySpec(vs, 0, lastType)
					for i, ident := range vs.Names {
						if ident.Name == "_" {
							continue
						}
						typeName, value, read := classifySpec(vs, i, lastType)
						if !wanted[typeName] {
							continue
						}
						if read {
							declared[typeName][ident.Name] = value
						} else {
							unreadable = append(unreadable, typeName+" "+ident.Name)
						}
					}
					lastType = specType
				}
				return true
			})
		}
	}
	return declared, unreadable
}

// classifySpec resolves, for the i-th name in vs, the declared type name and --
// when the value is a string literal or a conversion of one -- the string value
// it is declared with. The third result is false when no value could be read
// statically.
//
// The type comes from the explicit type annotation when present, otherwise from
// a conversion expression's callee (ReasonFoo = ReasonKind("foo")), otherwise --
// for a const spec with neither type nor expression -- from the previous spec.
// An untyped string literal carries no such type and is left unresolved.
func classifySpec(vs *ast.ValueSpec, i int, inherited string) (typeName, value string, read bool) {
	typeName = inherited
	if id, ok := vs.Type.(*ast.Ident); ok {
		typeName = id.Name
	}
	if i >= len(vs.Values) {
		return typeName, "", false
	}

	switch v := vs.Values[i].(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			if s, err := strconv.Unquote(v.Value); err == nil {
				return typeName, s, true
			}
		}
	case *ast.CallExpr:
		// A conversion T("x") names the type, so even a spec with no explicit
		// type annotation has one.
		if fun, ok := v.Fun.(*ast.Ident); ok && typeName == "" {
			typeName = fun.Name
		}
		if len(v.Args) == 1 {
			if arg, ok := v.Args[0].(*ast.BasicLit); ok && arg.Kind == token.STRING {
				if s, err := strconv.Unquote(arg.Value); err == nil {
					return typeName, s, true
				}
			}
		}
	}
	return typeName, "", false
}

func eventTypeStrings(ets []EventType) []string {
	out := make([]string, len(ets))
	for i, et := range ets {
		out[i] = string(et)
	}
	return out
}

func reasonKindStrings(kinds []ReasonKind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}
