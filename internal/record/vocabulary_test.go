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
// vocabulary complete by construction.
//
// It source-scans this package's non-test declarations for every constant
// typed EventType or ReasonKind and asserts each appears in EventTypes() /
// ReasonKinds(), and that neither accessor lists a value no declaration
// produces. It reads the declarations directly because Go cannot enumerate
// constants: this test is what links a constant's existence to the list a
// caller reads, so the list cannot drift and a new claim kind cannot be
// declared without the test naming it.
func TestVocabularyMatchesItsDeclarations(t *testing.T) {
	declared := scanTypedStringConstants(t, "EventType", "ReasonKind")

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

// scanTypedStringConstants parses every non-test .go file in this package
// directory and returns, per type name, the map from constant name to the
// string value it is declared with. Parsing the whole directory rather than a
// single file means a constant declared in a new file is still found.
func scanTypedStringConstants(t *testing.T, typeNames ...string) map[string]map[string]string {
	t.Helper()

	wanted := make(map[string]bool, len(typeNames))
	out := make(map[string]map[string]string, len(typeNames))
	for _, name := range typeNames {
		wanted[name] = true
		out[name] = map[string]string{}
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		name := fi.Name()
		return !fi.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	require.NoError(t, err, "parse package %s", "record")

	found := false
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			found = true
			ast.Inspect(file, func(n ast.Node) bool {
				gd, ok := n.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					return true
				}
				// A spec with no type repeats the previous spec's type, per Go's
				// const-group rules, so carry it forward within the group.
				lastType := ""
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					typeName := lastType
					if id, ok := vs.Type.(*ast.Ident); ok {
						typeName, lastType = id.Name, id.Name
					}
					if !wanted[typeName] {
						continue
					}
					for i, ident := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						value, err := strconv.Unquote(lit.Value)
						if err != nil {
							continue
						}
						out[typeName][ident.Name] = value
					}
				}
				return true
			})
		}
	}
	require.True(t, found, "no non-test .go files parsed in the record package")
	return out
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
