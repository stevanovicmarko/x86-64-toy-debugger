// Command progflow generates a Mermaid flowchart showing the internal
// control flow of every function in the debugger and CLI packages.
//
// Usage:
//
//	go run ./tools/progflow -out docs/program-flow.mmd
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// exitEdge is an outgoing connection from a node that hasn't been wired
// to a target yet. The optional label (e.g. "Yes", "No") is applied when
// the edge is finalized.
type exitEdge struct {
	fromID string
	label  string
}

// graphBuilder accumulates nodes and edges for a single function subgraph.
type graphBuilder struct {
	prefix  string // node ID prefix, e.g. "main_main"
	counter int
	nodes   []string // Mermaid node definitions
	edges   []string // Mermaid edge definitions
	fset    *token.FileSet
}

// node adds a node and returns its ID. Shape wraps the label:
//
//	"[" / "]"  → rectangle
//	"([" / "])" → rounded (stadium)
//	"{" / "}"  → diamond
func (g *graphBuilder) node(label, open, close string) string {
	g.counter++
	id := fmt.Sprintf("%s_%d", g.prefix, g.counter)
	g.nodes = append(g.nodes, fmt.Sprintf("        %s%s\"%s\"%s", id, open, sanitize(label), close))
	return id
}

// edge records a directed edge, optionally labelled.
func (g *graphBuilder) edge(from, to, label string) {
	if label != "" {
		g.edges = append(g.edges, fmt.Sprintf("    %s -->|\"%s\"| %s", from, sanitize(label), to))
	} else {
		g.edges = append(g.edges, fmt.Sprintf("    %s --> %s", from, to))
	}
}

// connectExits wires a set of pending exit edges to a target node.
func (g *graphBuilder) connectExits(exits []exitEdge, target string) {
	for _, e := range exits {
		g.edge(e.fromID, target, e.label)
	}
}

// walkStmts processes a slice of statements in sequence, chaining each
// statement's exits into the next statement's entry. Returns the entry
// node of the first statement and the dangling exits of the last.
func (g *graphBuilder) walkStmts(stmts []ast.Stmt) (entry string, exits []exitEdge) {
	for _, s := range stmts {
		e, sx := g.walkStmt(s)
		if e == "" {
			continue
		}
		if entry == "" {
			entry = e
		} else {
			g.connectExits(exits, e)
		}
		exits = sx
	}
	return entry, exits
}

// walkStmt handles a single statement, returning the entry node ID and
// any dangling exit edges.
func (g *graphBuilder) walkStmt(stmt ast.Stmt) (entry string, exits []exitEdge) {
	switch s := stmt.(type) {

	case *ast.ExprStmt:
		label := formatExpr(g.fset, s.X)
		id := g.node(label, "[", "]")
		return id, []exitEdge{{fromID: id}}

	case *ast.AssignStmt:
		lhs := formatExpr(g.fset, s.Lhs[0])
		rhs := formatExpr(g.fset, s.Rhs[0])
		tok := s.Tok.String()
		id := g.node(lhs+" "+tok+" "+rhs, "[", "]")
		return id, []exitEdge{{fromID: id}}

	case *ast.DeclStmt:
		label := "decl"
		if gd, ok := s.Decl.(*ast.GenDecl); ok && len(gd.Specs) > 0 {
			if vs, ok := gd.Specs[0].(*ast.ValueSpec); ok {
				label = "var " + vs.Names[0].Name
				if vs.Type != nil {
					label += " " + formatExpr(g.fset, vs.Type)
				}
			}
		}
		id := g.node(label, "[", "]")
		return id, []exitEdge{{fromID: id}}

	case *ast.DeferStmt:
		label := "defer " + formatExpr(g.fset, s.Call)
		id := g.node(label, "[", "]")
		return id, []exitEdge{{fromID: id}}

	case *ast.ReturnStmt:
		label := "return"
		if len(s.Results) > 0 {
			parts := make([]string, len(s.Results))
			for i, r := range s.Results {
				parts[i] = formatExpr(g.fset, r)
			}
			label += " " + strings.Join(parts, ", ")
		}
		id := g.node(label, "([", "])")
		return id, nil // terminal — no exits

	case *ast.BranchStmt:
		id := g.node(s.Tok.String(), "([", "])")
		return id, nil // terminal — no exits

	case *ast.IfStmt:
		cond := formatExpr(g.fset, s.Cond)
		condID := g.node(cond, "{", "}")

		thenEntry, thenExits := g.walkStmts(s.Body.List)
		if thenEntry != "" {
			g.edge(condID, thenEntry, "Yes")
		} else {
			thenExits = []exitEdge{{fromID: condID, label: "Yes"}}
		}

		var elseExits []exitEdge
		if s.Else != nil {
			elseEntry, ex := g.walkStmt(s.Else)
			if elseEntry != "" {
				g.edge(condID, elseEntry, "No")
				elseExits = ex
			} else {
				elseExits = []exitEdge{{fromID: condID, label: "No"}}
			}
		} else {
			elseExits = []exitEdge{{fromID: condID, label: "No"}}
		}

		return condID, append(thenExits, elseExits...)

	case *ast.ForStmt:
		var condLabel string
		if s.Cond != nil {
			condLabel = formatExpr(g.fset, s.Cond)
		} else {
			condLabel = "loop"
		}
		condID := g.node(condLabel, "{", "}")

		bodyEntry, bodyExits := g.walkStmts(s.Body.List)
		if bodyEntry != "" {
			g.edge(condID, bodyEntry, "Yes")
			// Back-edge: body exits loop back to condition.
			g.connectExits(bodyExits, condID)
		}

		if s.Cond != nil {
			return condID, []exitEdge{{fromID: condID, label: "No"}}
		}
		// Infinite loop — no natural exit.
		return condID, nil

	case *ast.SwitchStmt:
		tag := "switch"
		if s.Tag != nil {
			tag = formatExpr(g.fset, s.Tag)
		}
		switchID := g.node(tag, "{", "}")

		var allExits []exitEdge
		for _, clause := range s.Body.List {
			cc := clause.(*ast.CaseClause)
			caseLabel := "default"
			if len(cc.List) > 0 {
				parts := make([]string, len(cc.List))
				for i, e := range cc.List {
					parts[i] = formatExpr(g.fset, e)
				}
				caseLabel = strings.Join(parts, ", ")
			}

			caseEntry, caseExits := g.walkStmts(cc.Body)
			if caseEntry != "" {
				g.edge(switchID, caseEntry, caseLabel)
				allExits = append(allExits, caseExits...)
			} else {
				allExits = append(allExits, exitEdge{fromID: switchID, label: caseLabel})
			}
		}
		return switchID, allExits

	case *ast.TypeSwitchStmt:
		switchID := g.node("type switch", "{", "}")
		var allExits []exitEdge
		for _, clause := range s.Body.List {
			cc := clause.(*ast.CaseClause)
			caseLabel := "default"
			if len(cc.List) > 0 {
				parts := make([]string, len(cc.List))
				for i, e := range cc.List {
					parts[i] = formatExpr(g.fset, e)
				}
				caseLabel = strings.Join(parts, ", ")
			}
			caseEntry, caseExits := g.walkStmts(cc.Body)
			if caseEntry != "" {
				g.edge(switchID, caseEntry, caseLabel)
				allExits = append(allExits, caseExits...)
			} else {
				allExits = append(allExits, exitEdge{fromID: switchID, label: caseLabel})
			}
		}
		return switchID, allExits

	case *ast.BlockStmt:
		return g.walkStmts(s.List)

	case *ast.RangeStmt:
		label := "range " + formatExpr(g.fset, s.X)
		condID := g.node(label, "{", "}")
		bodyEntry, bodyExits := g.walkStmts(s.Body.List)
		if bodyEntry != "" {
			g.edge(condID, bodyEntry, "each")
			g.connectExits(bodyExits, condID)
		}
		return condID, []exitEdge{{fromID: condID, label: "done"}}

	default:
		// Catch-all for unhandled statement types (send, go, select, etc.)
		label := fmt.Sprintf("%T", stmt)
		id := g.node(label, "[", "]")
		return id, []exitEdge{{fromID: id}}
	}
}

// subgraph holds the Mermaid output pieces for one function.
type subgraph struct {
	id    string   // e.g. "main_main"
	title string   // e.g. "main()"
	nodes []string // node definitions (go inside subgraph block)
	edges []string // edge definitions (go outside subgraph block)
}

// analyzePackage parses a package directory and returns a subgraph for
// every function defined in it.
func analyzePackage(fset *token.FileSet, dir, pkgPrefix string) ([]subgraph, error) {
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		return nil, err
	}

	var subgraphs []subgraph

	for _, name := range pkg.GoFiles {
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			funcName := fn.Name.Name
			sgID := pkgPrefix + "_" + funcName
			title := funcName + "()"
			if pkgPrefix != "main" {
				title = pkgPrefix + "." + title
			}

			gb := &graphBuilder{
				prefix: sgID,
				fset:   fset,
			}

			gb.walkStmts(fn.Body.List)

			subgraphs = append(subgraphs, subgraph{
				id:    sgID,
				title: title,
				nodes: gb.nodes,
				edges: gb.edges,
			})
		}
	}

	return subgraphs, nil
}

// writeMermaid assembles the final flowchart TD from all subgraphs.
// Edges are placed outside subgraph blocks to avoid Mermaid rendering issues.
func writeMermaid(outPath string, sgs []subgraph) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString("flowchart TD\n")

	// Write subgraph blocks (nodes only).
	for _, sg := range sgs {
		fmt.Fprintf(&b, "    subgraph %s[\"%s\"]\n", sg.id, sg.title)
		for _, n := range sg.nodes {
			b.WriteString(n)
			b.WriteByte('\n')
		}
		b.WriteString("    end\n")
	}

	b.WriteByte('\n')

	// Write all edges outside subgraphs.
	for _, sg := range sgs {
		for _, e := range sg.edges {
			b.WriteString(e)
			b.WriteByte('\n')
		}
	}

	return os.WriteFile(outPath, []byte(b.String()), 0o644)
}

// formatExpr renders an AST expression to a readable string using go/printer.
// Newlines and tabs are collapsed to spaces so labels stay on one Mermaid line.
func formatExpr(fset *token.FileSet, expr ast.Expr) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, expr); err != nil {
		return "?"
	}
	s := buf.String()
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

// sanitize escapes characters that conflict with Mermaid syntax.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "\"", "#quot;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func main() {
	outPath := flag.String("out", "docs/program-flow.mmd", "output path for the Mermaid flowchart")
	flag.Parse()

	fset := token.NewFileSet()

	cliGraphs, err := analyzePackage(fset, "cmd/toydbg", "main")
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing cmd/toydbg:", err)
		os.Exit(1)
	}

	dbgGraphs, err := analyzePackage(fset, "debugger", "debugger")
	if err != nil {
		fmt.Fprintln(os.Stderr, "parsing debugger:", err)
		os.Exit(1)
	}

	all := append(cliGraphs, dbgGraphs...)

	if err := writeMermaid(*outPath, all); err != nil {
		fmt.Fprintln(os.Stderr, "writing output:", err)
		os.Exit(1)
	}

	fmt.Printf("wrote %d function subgraphs to %s\n", len(all), *outPath)
}
