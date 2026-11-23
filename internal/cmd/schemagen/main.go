// Command schemagen generates the protocol types from the vendored schema.
//
// The generated output is committed, so that go get needs no generator, and CI
// runs the generator and fails if the tree changes.
//
// Usage:
//
//	go run ./internal/cmd/schemagen            # write the generated files
//	go run ./internal/cmd/schemagen -check     # fail if they are not up to date
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

func main() {
	check := flag.Bool("check", false, "verify the committed output is up to date instead of writing it")
	root := flag.String("root", ".", "the repository root")
	flag.Parse()

	if err := run(*root, *check); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
}

// The repository files the generator reads and writes, relative to the root.
const (
	provenancePath = "schema/provenance.json"
	schemaPath     = "schema/schema.json"
	manifestPath   = "schema/manifest.json"
	metaPath       = "schema/meta.json"
)

func run(root string, check bool) error {
	repository := os.DirFS(root)
	outputs, err := generate(repository)
	if err != nil {
		return err
	}
	if check {
		return verify(repository, outputs)
	}
	return write(root, outputs)
}

// generate produces every output, keyed by its path relative to the root.
func generate(repository fs.FS) (map[string][]byte, error) {
	schemaTag, err := readSchemaTag(repository)
	if err != nil {
		return nil, err
	}
	rawSchema, err := fs.ReadFile(repository, schemaPath)
	if err != nil {
		return nil, err
	}
	doc, err := loadDocument(rawSchema)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", schemaPath, err)
	}

	rawManifest, err := fs.ReadFile(repository, manifestPath)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if decodeErr := json.Unmarshal(rawManifest, &manifest); decodeErr != nil {
		return nil, fmt.Errorf("%s: %w", manifestPath, decodeErr)
	}

	rawMeta, err := fs.ReadFile(repository, metaPath)
	if err != nil {
		return nil, err
	}
	table, err := loadMethodTable(rawMeta)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", metaPath, err)
	}
	methods, err := planMethods(doc, table)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", metaPath, err)
	}
	methodSource, err := emitMethods(methods, table)
	if err != nil {
		return nil, err
	}

	plan, err := newPlanner(doc).Plan(&manifest, schemaTag)
	if err != nil {
		return nil, err
	}
	source, err := emit(plan)
	if err != nil {
		return nil, err
	}
	roundTripSource, err := emitRoundTrip(plan)
	if err != nil {
		return nil, err
	}

	return map[string][]byte{
		"schema.gen.go":         source,
		"methods.gen.go":        methodSource,
		"roundtrip.gen_test.go": roundTripSource,
		"schema/exported.txt":   exportedList(plan),
	}, nil
}

func readSchemaTag(repository fs.FS) (string, error) {
	data, err := fs.ReadFile(repository, provenancePath)
	if err != nil {
		return "", err
	}
	var source struct {
		Schema struct {
			Tag string `json:"tag"`
		} `json:"schema"`
	}
	if decodeErr := json.Unmarshal(data, &source); decodeErr != nil {
		return "", fmt.Errorf("%s: %w", provenancePath, decodeErr)
	}
	if source.Schema.Tag == "" {
		return "", fmt.Errorf("%s: schema.tag is required", provenancePath)
	}
	return source.Schema.Tag, nil
}

func verify(repository fs.FS, outputs map[string][]byte) error {
	for _, path := range sortedKeys(outputs) {
		got, err := fs.ReadFile(repository, path)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, outputs[path]) {
			return fmt.Errorf("%s is not what the generator produces; run go generate", path)
		}
	}
	return nil
}

// write stages every output before replacing any, so a failure to produce one
// leaves the committed files as they were. Each replacement is a rename, which
// cannot leave a file half written; a rename that fails stops the rest, and no
// staged file is left behind either way.
func write(root string, outputs map[string][]byte) error {
	staged := make(map[string]string, len(outputs))
	discard := func(failure error) error {
		for _, temporary := range staged {
			if err := os.Remove(temporary); err != nil && !errors.Is(err, fs.ErrNotExist) {
				failure = errors.Join(failure, err)
			}
		}
		return failure
	}
	for _, path := range sortedKeys(outputs) {
		target := filepath.Join(root, filepath.FromSlash(path))
		temporary := target + ".tmp"
		// Committed generated source must be readable in module archives.
		//nolint:gosec // G306: these are world-readable source files, not secrets.
		if err := os.WriteFile(temporary, outputs[path], 0o644); err != nil {
			return discard(err)
		}
		staged[target] = temporary
	}
	for _, target := range sortedKeys(staged) {
		if err := os.Rename(staged[target], target); err != nil {
			return discard(err)
		}
	}
	return nil
}

// exportedList records the exported surface generation is responsible for, so
// that widening the API is a reviewable diff and a test can hold the package's
// actual exports against it.
func exportedList(plan *Plan) []byte {
	var out bytes.Buffer
	out.WriteString("# Written by internal/cmd/schemagen. Do not edit.\n")
	out.WriteString("#\n")
	out.WriteString("# Every exported Go name generation produces, one per line, sorted. Generation\n")
	fmt.Fprintf(&out, "# covers %d of the schema's definitions; exported_test.go holds the package's\n",
		len(plan.Closure))
	out.WriteString("# exports against this list.\n")
	for _, name := range plan.exportedNames() {
		out.WriteString(name)
		out.WriteString("\n")
	}
	return out.Bytes()
}

// exportedNames returns every exported Go name the plan produces, sorted.
func (p *Plan) exportedNames() []string {
	var names []string
	for _, def := range p.Defs {
		if !def.exported() {
			continue
		}
		names = append(names, def.GoName)
		switch body := def.Body.(type) {
		case *stringEnumBody:
			names = append(names, body.constantNames()...)
		case *numberEnumBody:
			names = append(names, body.constantNames()...)
		}
	}
	slices.Sort(names)
	return names
}
