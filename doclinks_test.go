package acp_test

import (
	"go/ast"
	"go/doc"
	"go/doc/comment"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// unresolvedDocLink matches bracketed text that go/doc left as plain text: a
// doc link it could not resolve. Brackets right after a word are an index or a
// map type, and brackets before a parenthesis are prose quoting Markdown.
var unresolvedDocLink = regexp.MustCompile(`(?:^|[^\w\]])(\[\*?[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*\])(?:[^(]|$)`)

// A renamed identifier leaves its old doc links rendering as plain text, which
// no compiler or linter reports. This holds every doc comment in the module to
// names that still exist.
//
// go/doc resolves only exported names, which is what a reader of the published
// documentation sees. The comment on an unexported declaration, or anywhere in a
// command, is read in the source instead, so a link there may name anything the
// package declares.
func TestEveryDocLinkResolves(t *testing.T) {
	for dir, files := range goPackages(t) {
		fset := token.NewFileSet()
		var parsed []*ast.File
		for _, file := range files {
			syntax, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			parsed = append(parsed, syntax)
		}
		pkg, err := doc.NewFromFiles(fset, parsed, "github.com/Tangerg/acp/"+dir, doc.AllDecls|doc.PreserveAST)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		declared := declaredNames(parsed)
		published := pkg.Name != "main"
		for owner, text := range docComments(pkg) {
			for _, link := range unresolvedLinks(pkg.Parser().Parse(text).Content) {
				if (!published || !isExportedPath(owner)) && declared[strings.Trim(link, "[*]")] {
					continue
				}
				t.Errorf("%s: the doc comment of %s links %s, which does not resolve", dir, owner, link)
			}
		}
	}
}

// declaredNames is every name a package declares, with methods and fields
// qualified by their type, as a doc link spells them.
func declaredNames(files []*ast.File) map[string]bool {
	names := make(map[string]bool)
	for _, file := range files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				names[receiverPrefix(decl.Recv)+decl.Name.Name] = true
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					declareSpec(names, spec)
				}
			}
		}
	}
	return names
}

func receiverPrefix(receiver *ast.FieldList) string {
	if receiver == nil || len(receiver.List) == 0 {
		return ""
	}
	typ := receiver.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	switch generic := typ.(type) {
	case *ast.IndexExpr:
		typ = generic.X
	case *ast.IndexListExpr:
		typ = generic.X
	}
	if ident, ok := typ.(*ast.Ident); ok {
		return ident.Name + "."
	}
	return ""
}

func declareSpec(names map[string]bool, spec ast.Spec) {
	switch spec := spec.(type) {
	case *ast.ValueSpec:
		for _, name := range spec.Names {
			names[name.Name] = true
		}
	case *ast.TypeSpec:
		names[spec.Name.Name] = true
		var members *ast.FieldList
		switch typ := spec.Type.(type) {
		case *ast.StructType:
			members = typ.Fields
		case *ast.InterfaceType:
			members = typ.Methods
		}
		if members == nil {
			return
		}
		for _, member := range members.List {
			for _, name := range member.Names {
				names[spec.Name.Name+"."+name.Name] = true
			}
		}
	}
}

func isExportedPath(owner string) bool {
	if strings.HasPrefix(owner, "package ") {
		return true
	}
	for part := range strings.SplitSeq(owner, ".") {
		if !token.IsExported(strings.TrimPrefix(part, "*")) {
			return false
		}
	}
	return true
}

// goPackages lists the module's non-test Go files by package directory.
func goPackages(t *testing.T) map[string][]string {
	t.Helper()
	packages := make(map[string][]string)
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != "." && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "testdata" ||
			entry.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			packages[filepath.ToSlash(filepath.Dir(path))] = append(packages[filepath.ToSlash(filepath.Dir(path))], path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return packages
}

// docComments yields every doc comment in a package with the declaration it
// documents.
func docComments(pkg *doc.Package) func(yield func(owner, text string) bool) {
	return func(yield func(owner, text string) bool) {
		if !yield("package "+pkg.Name, pkg.Doc) {
			return
		}
		values := slices.Concat(pkg.Consts, pkg.Vars)
		funcs := slices.Clone(pkg.Funcs)
		for _, typ := range pkg.Types {
			if !yield(typ.Name, typ.Doc) {
				return
			}
			values = slices.Concat(values, typ.Consts, typ.Vars)
			funcs = slices.Concat(funcs, typ.Funcs, typ.Methods)
			for _, spec := range typ.Decl.Specs {
				for owner, text := range fieldComments(typ.Name, spec) {
					if !yield(owner, text) {
						return
					}
				}
			}
		}
		for _, value := range values {
			if !yield(strings.Join(value.Names, ", "), value.Doc) {
				return
			}
		}
		for _, fn := range funcs {
			owner := fn.Name
			if fn.Recv != "" {
				owner = fn.Recv + "." + fn.Name
			}
			if !yield(owner, fn.Doc) {
				return
			}
		}
	}
}

// fieldComments yields the comments on a struct's fields and an interface's
// methods, which godoc renders with the type.
func fieldComments(typeName string, spec ast.Spec) func(yield func(owner, text string) bool) {
	return func(yield func(owner, text string) bool) {
		typeSpec, ok := spec.(*ast.TypeSpec)
		if !ok {
			return
		}
		var fields *ast.FieldList
		switch typ := typeSpec.Type.(type) {
		case *ast.StructType:
			fields = typ.Fields
		case *ast.InterfaceType:
			fields = typ.Methods
		default:
			return
		}
		for _, field := range fields.List {
			if field.Doc == nil {
				continue
			}
			owner := typeName
			if len(field.Names) > 0 {
				owner += "." + field.Names[0].Name
			}
			if !yield(owner, field.Doc.Text()) {
				return
			}
		}
	}
}

func unresolvedLinks(blocks []comment.Block) []string {
	var links []string
	for _, block := range blocks {
		switch block := block.(type) {
		case *comment.Paragraph:
			links = append(links, unresolvedInText(block.Text)...)
		case *comment.Heading:
			links = append(links, unresolvedInText(block.Text)...)
		case *comment.List:
			for _, item := range block.Items {
				links = append(links, unresolvedLinks(item.Content)...)
			}
		}
	}
	return links
}

func unresolvedInText(texts []comment.Text) []string {
	var links []string
	for _, text := range texts {
		switch text := text.(type) {
		case comment.Plain:
			for _, match := range unresolvedDocLink.FindAllStringSubmatch(string(text), -1) {
				links = append(links, match[1])
			}
		case *comment.Link:
			links = append(links, unresolvedInText(text.Text)...)
		}
	}
	return links
}
