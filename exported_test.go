package acp_test

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// handWritten is every exported name in this package that generation does not
// own, with the file it lives in and why it is not generated.
//
// It is a closed list, and that is the point. The exported surface of this
// package is meant to be the schema's, so a name arriving any other way has to be
// a decision somebody wrote down rather than a type that appeared because it was
// convenient. Adding a line here is cheap; adding one without noticing is what
// this prevents.
var handWritten = map[string]string{
	// The wire vocabulary the schema does not name.
	"CurrentProtocolVersion": "version.go — the protocol version this package speaks, which is not a schema definition",
	"Meta":                   "meta.go — the Go type of the _meta property, which the schema leaves as a free-form object",
	"Opt":                    "opt.go — the presence-aware optional the schema's absent/null/present distinction needs",
	"OptNull":                "opt.go — constructs the null state",
	"OptValue":               "opt.go — constructs the present state",
	"PeerInfo": "peer.go — the snapshot initialize negotiates, which the schema has no single " +
		"type for: it is two capability objects and two identifications, one of each per peer",

	// The two peers, their configuration, and their connections. The schema
	// describes messages; who sends them and how a connection is owned is this
	// package's to design.
	"Client":       "client.go — the editor's side: it holds the handlers and may hold many connections",
	"ClientConfig": "client.go — construction settings, with handler fields grouped by capability",
	"ClientConn":   "client.go — one logical connection, after initialize",
	"NewClient":    "client.go — construction, which fails rather than accept an advertisement it cannot serve",
	"Agent":        "agent.go — the agent's side: it serves rather than drives",
	"AgentConfig":  "agent.go — construction settings, mirroring ClientConfig in structure and not in content",
	"AgentConn":    "agent.go — one logical connection, which has no session-creating methods",
	"NewAgent":     "agent.go — construction, which fails without the baseline handlers",

	// The conversation, seen from each side. The schema has a session identifier;
	// a handle that binds it is this package's.
	"ClientSession":  "session.go — one conversation as the client sees it: it drives turns",
	"AgentSession":   "session.go — the same conversation as the agent sees it: a different set of operations",
	"TerminalHandle": "workspace.go — one terminal, binding both identifiers its five methods need",
	"TerminalHandlers": "client.go — the terminal capability's complete handler set, which is all five " +
		"or none because the capability is one boolean",

	// The transport boundary.
	"Transport":             "transport.go — what a connection is established over",
	"Connection":            "transport.go — an established bidirectional message stream",
	"NewInMemoryTransports": "transport_memory.go — two transports connected to each other",
	"NewStdioTransport":     "transport_stdio.go — the agent's side of a local connection",
	"NewIOTransport":        "transport_stdio.go — newline-delimited JSON over a closeable stream pair",
	"NewCommandTransport":   "transport_stdio.go — the client's side: it starts the agent as a subprocess",
	"CommandConfig":         "transport_stdio.go — the command to run and how long it is given to stop",

	// The extension boundary, which is how anyone implements an ACP extension at
	// all. The schema's Ext* definitions are unconstrained placeholders and carry
	// no method name, so these are not projections of them.
	"ExtRequest":      "extension.go — a request for a method the specification does not define",
	"ExtNotification": "extension.go — a notification for one",

	// The sentinels, which exist only where control flow needs one.
	"ErrAuthRequired":     "error.go — how an agent says authenticate first, which is control flow",
	"ErrRequestCancelled": "error.go — a peer's -32800, which does not prove anybody cancelled anything",
	"ErrConnectionClosed": "error.go — a local state rather than a wire error: nobody sent it",
	"ErrPromptInProgress": "session.go — one prompt at a time per session, because session/cancel names no turn",
	"ErrNotInitialized":   "agent.go — an agent connection exists before the handshake it does not initiate",
	"ErrTerminalReleased": "workspace.go — a released terminal identifier is the client's to reuse, so the handle is spent",
}

// The manifest decides generation scope and schema/exported.txt records what that
// scope became. This holds the package's actual exports against it, in both
// directions: a generated name that vanished, and an exported name that reached a
// generated surface some other way.
//
// Without it the manifest is a promise about an artifact nothing compares to the
// package, and gorelease will hold the module to whatever is exported at the
// first tag whether or not anyone meant it.
func TestExportedSurfaceIsTheGeneratedClosurePlusWhatIsWrittenByHand(t *testing.T) {
	generated := readExportedList(t)
	exported := parseExportedNames(t)

	for _, name := range generated {
		if !slices.Contains(exported, name) {
			t.Errorf("schema/exported.txt names %s, which the package does not export; "+
				"run go generate ./...", name)
		}
	}

	for _, name := range exported {
		switch {
		case slices.Contains(generated, name):
			if reason, ok := handWritten[name]; ok {
				t.Errorf("%s is both generated and listed as hand-written (%s); one of the two is wrong",
					name, reason)
			}
		case handWritten[name] != "":
		default:
			t.Errorf("the package exports %s, which is neither in schema/exported.txt nor in the "+
				"hand-written list; either add it to the manifest or record why it is written by hand", name)
		}
	}

	for name, reason := range handWritten {
		if !slices.Contains(exported, name) {
			t.Errorf("the hand-written list names %s (%s), which the package does not export", name, reason)
		}
	}
}

func readExportedList(t *testing.T) []string {
	t.Helper()
	file, err := os.Open(filepath.Join("schema", "exported.txt"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer file.Close() //nolint:errcheck // a read-only file's Close cannot report anything a test can act on.

	var names []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// A name may carry the UNSTABLE marker the schema put on its definition,
		// which the compatibility review reads and this test does not.
		names = append(names, strings.Fields(line)[0])
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("schema/exported.txt is empty")
	}
	return names
}

// parseExportedNames collects the package's exported top-level declarations.
//
// Methods are deliberately not collected. They belong to the type they are on,
// and a type this test has already accounted for cannot acquire a method by
// accident — whereas a whole type can appear in a new file without anyone
// deciding it should be published.
//
// The files are parsed rather than type-checked, and build constraints are
// ignored. Nothing in this package is platform-guarded, and a surface that
// existed only on one operating system would be a defect this test should report
// rather than accommodate.
func parseExportedNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read directory: %v", err)
	}

	fileSet := token.NewFileSet()
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if file.Name.Name != "acp" {
			t.Fatalf("%s declares package %s", name, file.Name.Name)
		}
		for _, declaration := range file.Decls {
			names = append(names, exportedNamesOf(declaration)...)
		}
	}
	if len(names) == 0 {
		t.Fatal("no exported declarations found; the parse walked nothing")
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func exportedNamesOf(declaration ast.Decl) []string {
	var names []string
	keep := func(name *ast.Ident) {
		if name.IsExported() {
			names = append(names, name.Name)
		}
	}
	switch declaration := declaration.(type) {
	case *ast.FuncDecl:
		if declaration.Recv == nil {
			keep(declaration.Name)
		}
	case *ast.GenDecl:
		for _, spec := range declaration.Specs {
			switch spec := spec.(type) {
			case *ast.TypeSpec:
				keep(spec.Name)
			case *ast.ValueSpec:
				for _, name := range spec.Names {
					keep(name)
				}
			}
		}
	}
	return names
}
