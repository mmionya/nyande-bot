package resources

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCatalogHasNoDuplicateKeys(t *testing.T) {
	file, err := files.Open("strings.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		name := key.(string)
		if seen[name] {
			t.Errorf("duplicate resource key %q", name)
		}
		seen[name] = true
		var value string
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodeReferencesExistingResources(t *testing.T) {
	positions := token.NewFileSet()
	for _, directory := range []string{"../cmd", "../internal"} {
		err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(positions, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "Get" && selector.Sel.Name != "Format") {
					return true
				}
				owner, ok := selector.X.(*ast.Ident)
				if !ok || owner.Name != "resources" {
					return true
				}
				literal, ok := call.Args[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true // Dynamic keys are covered by the feature's behavior tests.
				}
				key, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Error(err)
				} else if _, exists := values[key]; !exists {
					t.Errorf("%s: missing resource %q", positions.Position(literal.Pos()), key)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
