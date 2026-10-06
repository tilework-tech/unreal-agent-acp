package emit

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"testing"

	"github.com/tilework-tech/unreal-agent-acp/tools/acpgen/internal/load"
)

func typeCheck(t *testing.T, dir string) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	config := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	if _, err := config.Check(PackageName, fset, files, nil); err != nil {
		t.Fatalf("generated code does not type-check: %v", err)
	}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			block, ok := node.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for index, statement := range block.List {
				if _, isReturn := statement.(*ast.ReturnStmt); isReturn && index != len(block.List)-1 {
					t.Errorf("%s: unreachable code after return", fset.Position(block.List[index+1].Pos()))
				}
			}
			return true
		})
	}
}

func TestPrimitiveUnionVariantsHaveNoDeadCode(t *testing.T) {
	schema := &load.Schema{Defs: map[string]*load.Definition{
		"Value": {AnyOf: []*load.Definition{
			{Type: "string", Title: "String"},
			{Type: "integer", Title: "Integer"},
			{Type: "object", Title: "Object", Properties: map[string]*load.Definition{"x": {Type: "string"}}},
		}},
	}}
	meta := &load.Meta{Version: 1}
	dir := t.TempDir()
	if err := WriteConstantsJen(dir, meta); err != nil {
		t.Fatal(err)
	}
	if err := WriteTypesJen(dir, schema, meta); err != nil {
		t.Fatal(err)
	}
	typeCheck(t, dir)
}

func TestRequestUnionWithSharedPropertiesCompiles(t *testing.T) {
	str := &load.Definition{Type: "string"}
	schema := &load.Schema{Defs: map[string]*load.Definition{
		"AskRequest": {
			Type:       "object",
			XSide:      "client",
			XMethod:    "ask",
			Properties: map[string]*load.Definition{"message": str},
			Required:   []string{"message"},
			AnyOf: []*load.Definition{
				{
					Type:       "object",
					Title:      "form",
					Properties: map[string]*load.Definition{"mode": {Type: "string", Const: "form"}, "fields": str},
					Required:   []string{"mode", "fields"},
				},
				{
					Type:       "object",
					Title:      "url",
					Properties: map[string]*load.Definition{"mode": {Type: "string", Const: "url"}, "url": str},
					Required:   []string{"mode", "url"},
				},
			},
		},
		"AskResponse": {Type: "object", XSide: "client", XMethod: "ask", Properties: map[string]*load.Definition{"ok": {Type: "boolean"}}},
	}}
	meta := &load.Meta{Version: 1, ClientMethods: map[string]string{"ask": "ask"}}

	dir := t.TempDir()
	if err := WriteConstantsJen(dir, meta); err != nil {
		t.Fatal(err)
	}
	if err := WriteTypesJen(dir, schema, meta); err != nil {
		t.Fatal(err)
	}
	if err := WriteHelpersJen(dir, schema, meta); err != nil {
		t.Fatal(err)
	}
	typeCheck(t, dir)
}
