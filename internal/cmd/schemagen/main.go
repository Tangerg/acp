// Command schemagen generates the protocol types from the vendored schema.
//
// The generated output is committed, so that go get needs no generator and
// pkg.go.dev has something to document, and CI runs the generator and fails if
// the tree changes — the same promise as go mod tidy -diff.
//
// Usage:
//
//	go run ./internal/cmd/schemagen            # write the generated files
//	go run ./internal/cmd/schemagen -check     # fail if they are not up to date
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// schemaTag is the upstream release the vendored schema is pinned to. It is
// recorded in the generated header so that a reader of the generated code knows
// which grammar it implements without going looking.
const schemaTag = "schema-v1.21.0"

func main() {
	check := flag.Bool("check", false, "verify the committed output is up to date instead of writing it")
	root := flag.String("root", ".", "the repository root")
	flag.Parse()

	if err := run(*root, *check); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
}

func run(root string, check bool) error {
	schemaPath := filepath.Join(root, "schema", "schema.json")
	rawSchema, readErr := readRepositoryFile(schemaPath)
	if readErr != nil {
		return readErr
	}
	doc, docErr := loadDocument(rawSchema)
	if docErr != nil {
		return fmt.Errorf("%s: %w", schemaPath, docErr)
	}
	if err := checkKnownKeywords(rawSchema); err != nil {
		return fmt.Errorf("%s: %w", schemaPath, err)
	}

	manifestPath := filepath.Join(root, "schema", "manifest.json")
	rawManifest, readErr := readRepositoryFile(manifestPath)
	if readErr != nil {
		return readErr
	}
	var manifest Manifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return fmt.Errorf("%s: %w", manifestPath, err)
	}

	metaPath := filepath.Join(root, "schema", "meta.json")
	rawMeta, readErr := readRepositoryFile(metaPath)
	if readErr != nil {
		return readErr
	}
	table, tableErr := loadMethodTable(rawMeta)
	if tableErr != nil {
		return fmt.Errorf("%s: %w", metaPath, tableErr)
	}
	methods, methodsErr := planMethods(doc, table)
	if methodsErr != nil {
		return fmt.Errorf("%s: %w", metaPath, methodsErr)
	}
	methodSource, methodEmitErr := emitMethods(methods, table)
	if methodEmitErr != nil {
		return methodEmitErr
	}

	plan, planErr := newPlanner(doc).Plan(&manifest, schemaTag)
	if planErr != nil {
		return planErr
	}
	source, emitErr := emit(plan)
	if emitErr != nil {
		return emitErr
	}

	outputs := map[string][]byte{
		filepath.Join(root, "schema.gen.go"):          source,
		filepath.Join(root, "methods.gen.go"):         methodSource,
		filepath.Join(root, "schema", "exported.txt"): exportedList(plan),
	}
	if check {
		return verify(outputs)
	}
	for path, want := range outputs {
		if err := os.WriteFile(path, want, 0o600); err != nil {
			return err
		}
		//nolint:gosec // committed generated source must be readable in module archives.
		if err := os.Chmod(path, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func verify(outputs map[string][]byte) error {
	paths := slices.Sorted(maps.Keys(outputs))
	for _, path := range paths {
		got, err := readRepositoryFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, outputs[path]) {
			return fmt.Errorf("%s is not what the generator produces; run go generate", path)
		}
	}
	return nil
}

// readRepositoryFile reads a path this command built from its own flags. The
// paths are the repository's own files, not a caller's input, which is why the
// file-inclusion warning does not apply.
func readRepositoryFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is derived from -root, not from untrusted input.
	if err != nil {
		return nil, err
	}
	return data, nil
}

// exportedList records the exported surface generation is responsible for.
//
// It is the other half of the manifest's promise. The manifest decides scope and
// this records what that scope became, so that widening the API is a reviewable
// diff rather than a side effect — and so that a test can hold the package's
// actual exports against it and catch a name that reached a generated surface by
// hand.
//
// It is a plain list rather than a document because a test reads it, and because
// the interesting thing about it is the diff.
func exportedList(plan *Plan) []byte {
	var out bytes.Buffer
	out.WriteString("# Written by internal/cmd/schemagen. Do not edit.\n")
	out.WriteString("#\n")
	out.WriteString("# Every exported Go name generation produces, one per line, sorted. The\n")
	out.WriteString("# transitive $ref closure of manifest.json is ")
	fmt.Fprintf(&out, "%d of the schema's\n", len(plan.Closure))
	out.WriteString("# definitions; exported_test.go holds the package's exports against this list.\n")
	names := plan.goNames()
	slices.Sort(names)
	for _, name := range names {
		out.WriteString(name)
		out.WriteString("\n")
	}
	return out.Bytes()
}

// goNames returns every exported Go name the plan produces.
//
// The unexported ones are left out on purpose: this file is what the package's
// published surface is checked against, and JSON-RPC plumbing is generated
// precisely so that it is not part of that surface.
func (p *Plan) goNames() []string {
	var names []string
	for _, def := range p.Defs {
		if def.GoName != def.Ident {
			continue
		}
		names = append(names, def.GoName)
		for _, value := range def.Values {
			names = append(names, value.GoName)
		}
	}
	return names
}
