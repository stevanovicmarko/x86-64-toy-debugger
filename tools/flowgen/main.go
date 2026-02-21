// Command flowgen generates a Mermaid sequence diagram showing call flow
// between the CLI, debugger library, and OS layers.
//
// Usage:
//
//	go run ./tools/flowgen -out docs/code-flow.mmd
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// Arrow represents a single call from one layer to another.
type Arrow struct {
	From  string // participant name: "CLI", "debugger", "OS"
	To    string
	Label string // e.g. "Launch(program)"
}

// classifyCall decides how a selector expression (pkg.Func) maps to a
// diagram arrow. Returns an Arrow and true if the call should appear in
// the diagram, or false if it should be skipped.
func classifyCall(callerLayer, pkg, funcName string) (Arrow, bool) {
	switch {
	// CLI layer calling into the debugger library.
	case callerLayer == "CLI" && pkg == "debugger":
		return Arrow{From: "CLI", To: "debugger", Label: funcName}, true

	// Debugger layer calling OS-level packages (syscall, exec).
	case callerLayer == "debugger" && (pkg == "syscall" || pkg == "exec"):
		return Arrow{From: "debugger", To: "OS", Label: funcName}, true

	default:
		return Arrow{}, false
	}
}

func main() {
	outPath := flag.String("out", "docs/code-flow.mmd", "output path for the Mermaid diagram")
	flag.Parse()

	fset := token.NewFileSet()

	cliArrows, err := extractCalls(fset, "cmd/toydbg", "CLI")
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing cmd/toydbg:", err)
		os.Exit(1)
	}

	dbgArrows, err := extractCalls(fset, "debugger", "debugger")
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing debugger:", err)
		os.Exit(1)
	}

	arrows := append(cliArrows, dbgArrows...)

	if err := writeMermaid(*outPath, arrows); err != nil {
		fmt.Fprintln(os.Stderr, "writing output:", err)
		os.Exit(1)
	}

	fmt.Printf("wrote %d arrows to %s\n", len(arrows), *outPath)
}

// extractCalls parses a package directory and returns diagram arrows for
// every cross-layer selector call (e.g. debugger.Launch, syscall.Wait4).
//
// It uses go/build.ImportDir to resolve which .go files belong to the
// package for the current platform (respecting build tags), then parses
// each file individually with go/parser.ParseFile.
func extractCalls(fset *token.FileSet, dir, callerLayer string) ([]Arrow, error) {
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		return nil, err
	}

	var arrows []Arrow

	for _, name := range pkg.GoFiles {
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}

			pkgName := ident.Name
			funcName := sel.Sel.Name

			if a, keep := classifyCall(callerLayer, pkgName, funcName); keep {
				arrows = append(arrows, a)
			}

			return true
		})
	}

	return arrows, nil
}

// writeMermaid writes arrows as a Mermaid sequence diagram to outPath,
// creating parent directories as needed.
func writeMermaid(outPath string, arrows []Arrow) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString("sequenceDiagram\n")
	b.WriteString("    participant CLI\n")
	b.WriteString("    participant debugger\n")
	b.WriteString("    participant OS\n")
	b.WriteString("\n")

	for _, a := range arrows {
		fmt.Fprintf(&b, "    %s->>%s: %s()\n", a.From, a.To, a.Label)
	}

	return os.WriteFile(outPath, []byte(b.String()), 0o644)
}
