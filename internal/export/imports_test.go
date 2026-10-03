package export_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// phraseImportPath is the package that talks to the LLM. Design D7 makes
// "generated text is never an input to a decision" a property of the import
// graph rather than of review: only internal/export and cmd/notary may reach
// it, so no decision package can even name phrase.Paraphrase.
const phraseImportPath = "notary/internal/phrase"

// allowedPhraseImporters are the only packages permitted to import
// internal/phrase. cmd/notary wires the real client under --phrase;
// internal/export holds the Paraphraser seam and the Line field that carries
// the paraphrase; internal/phrase is the package itself (its external test
// package imports it, as every external test package must). No other package --
// and in particular no decision package -- may reach it.
var allowedPhraseImporters = map[string]bool{
	"notary/internal/export": true,
	"notary/cmd/notary":      true,
	"notary/internal/phrase": true,
}

// TestNoDecisionPackageImportsPhrase walks the module's own source, parsing the
// import block of every .go file (tests included), and asserts that the only
// packages importing internal/phrase are internal/export and cmd/notary. It
// parses files rather than shelling out to `go list`, so the check is hermetic
// and cannot be perturbed by the build cache.
//
// This guard is what makes D7 checkable. It is deliberately sensitive: a single
// blank import of internal/phrase in any other package trips it.
func TestNoDecisionPackageImportsPhrase(t *testing.T) {
	modulePath := readModulePath(t)
	root := "../.." // the module root sits two levels above internal/export

	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		importer := packagePath(modulePath, root, filepath.Dir(path))
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) != phraseImportPath {
				continue
			}
			if allowedPhraseImporters[importer] {
				continue
			}
			violations = append(violations, importer+" (imported in "+filepath.ToSlash(path)+")")
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, violations,
		"only %v may import %s; a decision package importing generated text breaks D7: %v",
		allowedPhraseImporters, phraseImportPath, violations)
}

// readModulePath returns the module path from go.mod, so the test does not
// hardcode the module name.
func readModulePath(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	require.NoError(t, err)
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("go.mod declares no module path")
	return ""
}

// packagePath maps a directory under root to the import path of the package it
// holds. External test packages (_test) share their directory's import path for
// this purpose, which is what we want: a decision package's own tests importing
// phrase is a violation too.
func packagePath(modulePath, root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return modulePath
	}
	return modulePath + "/" + rel
}
