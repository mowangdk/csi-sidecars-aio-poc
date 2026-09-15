/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// lifecycle-adapter changes only AIO entrypoints, never standalone commands.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
)

type edit struct {
	start, end int
	text       string
}
type adapter struct {
	source []byte
	set    *token.FileSet
	edits  []edit
}

func (a *adapter) text(n ast.Node) string {
	return string(a.source[a.set.Position(n.Pos()).Offset:a.set.Position(n.End()).Offset])
}
func (a *adapter) replace(n ast.Node, text string) {
	a.edits = append(a.edits, edit{a.set.Position(n.Pos()).Offset, a.set.Position(n.End()).Offset, text})
}
func (a *adapter) insert(pos token.Pos, text string) {
	offset := a.set.Position(pos).Offset
	a.edits = append(a.edits, edit{offset, offset, text})
}

// Tokens ignore comments, optional semicolons, and trailing commas. Parsing is
// always performed first, so this is not used to accept malformed Go syntax.
func tokens(source string) string {
	var s scanner.Scanner
	set := token.NewFileSet()
	s.Init(set.AddFile("", -1, len(source)), []byte(source), nil, 0)
	var out strings.Builder
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON || tok == token.COMMA {
			continue
		}
		fmt.Fprintf(&out, "%d:%s|", tok, lit)
	}
	return out.String()
}

func callName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return callName(e.X) + "." + e.Sel.Name
	}
	return ""
}
func expressionCall(stmt ast.Stmt) *ast.CallExpr {
	e, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return nil
	}
	c, _ := e.X.(*ast.CallExpr)
	return c
}
func terminal(c *ast.CallExpr) bool {
	if c == nil {
		return false
	}
	switch callName(c.Fun) {
	case "os.Exit", "klog.FlushAndExit", "klog.Fatal", "klog.Fatalf":
		return true
	}
	return false
}

func (a *adapter) exitError(c, log *ast.CallExpr) (string, error) {
	name := callName(c.Fun)
	args := func(items []ast.Expr) string {
		var values []string
		for _, item := range items {
			values = append(values, a.text(item))
		}
		return strings.Join(values, ", ")
	}
	if name == "klog.Fatalf" || name == "klog.Fatal" {
		log = c
	}
	if log == nil {
		return "", fmt.Errorf("process exit without adjacent diagnostic: %s", a.text(c))
	}
	cause := "nil"
	for _, arg := range log.Args {
		if a.text(arg) == "err" || a.text(arg) == "err.Error()" {
			cause = "err"
		}
	}
	var message string
	switch callName(log.Fun) {
	case "logger.Error", "klog.ErrorS":
		if len(log.Args) < 2 {
			return "", fmt.Errorf("invalid structured error diagnostic")
		}
		message = "fmt.Sprint(" + args(log.Args[1:]) + ")"
		cause = a.text(log.Args[0])
	case "klog.Errorf", "klog.Fatalf":
		message = "fmt.Sprintf(" + args(log.Args) + ")"
	case "klog.Error", "klog.Fatal":
		message = "fmt.Sprint(" + args(log.Args) + ")"
	default:
		return "", fmt.Errorf("unsupported exit diagnostic: %s", a.text(log))
	}
	return "csistartup.Error(ctx, " + message + ", " + cause + ")", nil
}

func (a *adapter) startup(block *ast.BlockStmt) error {
	for i, stmt := range block.List {
		if c := expressionCall(stmt); terminal(c) {
			var log *ast.CallExpr
			if i > 0 {
				log = expressionCall(block.List[i-1])
			}
			message, err := a.exitError(c, log)
			if err != nil {
				return err
			}
			a.replace(stmt, "return "+message)
			continue
		}
		switch s := stmt.(type) {
		case *ast.IfStmt:
			if err := a.startup(s.Body); err != nil {
				return err
			}
			if s.Else != nil {
				if other, ok := s.Else.(*ast.BlockStmt); ok {
					if err := a.startup(other); err != nil {
						return err
					}
				} else {
					if err := a.startup(&ast.BlockStmt{List: []ast.Stmt{s.Else}}); err != nil {
						return err
					}
				}
			}
		case *ast.GoStmt:
			fn, ok := s.Call.Fun.(*ast.FuncLit)
			if !ok || len(s.Call.Args) != 0 || len(fn.Body.List) != 3 {
				return fmt.Errorf("unrecognized startup goroutine: %s", a.text(s))
			}
			assignment, ok := fn.Body.List[1].(*ast.AssignStmt)
			if !ok || tokens(a.text(assignment)) != tokens("err := http.ListenAndServe(addr, mux)") {
				return fmt.Errorf("unrecognized startup service: %s", a.text(s))
			}
			logging := expressionCall(fn.Body.List[0])
			if logging == nil || !strings.HasPrefix(callName(logging.Fun), "klog.Info") && callName(logging.Fun) != "logger.Info" {
				return fmt.Errorf("unrecognized HTTP startup log")
			}
			check, ok := fn.Body.List[2].(*ast.IfStmt)
			if !ok || tokens(a.text(check.Cond)) != tokens("err != nil") || check.Else != nil || len(check.Body.List) == 0 || len(check.Body.List) > 2 {
				return fmt.Errorf("unrecognized HTTP failure handling")
			}
			exit := expressionCall(check.Body.List[len(check.Body.List)-1])
			if !terminal(exit) {
				return fmt.Errorf("HTTP failure is not terminal")
			}
			var diagnostic *ast.CallExpr
			if len(check.Body.List) == 2 {
				diagnostic = expressionCall(check.Body.List[0])
			}
			if _, err := a.exitError(exit, diagnostic); err != nil {
				return err
			}
			a.replace(s, `if err := scope.ServeHTTP(addr, mux, aioConfiguration.Configuration.ShutdownTimeout); err != nil { return err }`)
		case *ast.RangeStmt:
			if err := a.startup(s.Body); err != nil {
				return err
			}
		case *ast.ForStmt:
			if err := a.startup(s.Body); err != nil {
				return err
			}
		}
	}
	return nil
}

func adapt(source []byte, sidecar string) ([]byte, error) {
	if _, ok := lockNames[sidecar]; !ok {
		return nil, fmt.Errorf("unknown controller %q", sidecar)
	}
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "entrypoint.go", source, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	a := &adapter{source: source, set: set}
	var entry *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == sidecar+"_main" {
			entry = fn
		}
	}
	if entry == nil {
		return source, nil
	} // helpers in a separate source file
	if entry.Type.Results != nil {
		return nil, fmt.Errorf("entrypoint already adapted")
	}
	tailIndex := -1
	for i, stmt := range entry.Body.List {
		if decl, ok := stmt.(*ast.DeclStmt); ok {
			gen, ok := decl.Decl.(*ast.GenDecl)
			if ok && len(gen.Specs) > 0 {
				spec, ok := gen.Specs[0].(*ast.ValueSpec)
				if ok && len(spec.Names) == 1 && spec.Names[0].Name == "terminate" {
					tailIndex = i
					break
				}
			}
		}
	}
	if tailIndex < 0 {
		return nil, fmt.Errorf("missing recognized lifecycle tail")
	}
	tailStart := set.Position(entry.Body.List[tailIndex].Pos()).Offset
	tailEnd := set.Position(entry.Body.Rbrace).Offset
	matched := false
	for _, expected := range expectedTails(sidecar) {
		if tokens(string(source[tailStart:tailEnd])) == tokens(expected) {
			matched = true
			break
		}
	}
	if !matched {
		return nil, fmt.Errorf("%s lifecycle shape changed; review the complete tail before updating its fixture", sidecar)
	}
	a.edits = append(a.edits, edit{tailStart, tailEnd, adaptedWorkers[sidecar] + "\nreturn " + electionCall(sidecar) + "\n"})
	a.insert(entry.Type.Params.End(), " (result error)")
	a.insert(entry.Body.Lbrace+1, `
 scope := aioruntime.NewScope(ctx)
 ctx = scope.Context()
 defer func() { result = scope.Close(result) }()
`)
	startup := &ast.BlockStmt{}
	for _, stmt := range entry.Body.List[:tailIndex] {
		text := a.text(stmt)
		if tokens(text) == tokens("ctx := context.Background()") || tokens(text) == tokens("ctx := context.TODO()") {
			a.replace(stmt, "")
			continue
		}
		if call := expressionCall(stmt); call != nil && callName(call.Fun) == "connection.SetMaxGRPCLogLength" {
			if sidecar != "attacher" || tokens(a.text(call)) != tokens("connection.SetMaxGRPCLogLength(*maxGRPCLogLength)") {
				return nil, fmt.Errorf("unrecognized process-wide gRPC logging configuration")
			}
			// AIO configures this shared library global before launching runners.
			a.replace(stmt, "")
			continue
		}
		if branch, ok := stmt.(*ast.IfStmt); ok {
			cond := tokens(a.text(branch.Cond))
			if cond == tokens("standardflags.Configuration.ShowVersion") || cond == tokens("*showVersion") {
				if len(branch.Body.List) != 2 || branch.Else != nil ||
					tokens(a.text(branch.Body.List[0])) != tokens("fmt.Println(os.Args[0], version)") ||
					(tokens(a.text(branch.Body.List[1])) != tokens("return") && tokens(a.text(branch.Body.List[1])) != tokens("os.Exit(0)")) {
					return nil, fmt.Errorf("unrecognized version handling")
				}
				a.replace(stmt, "")
				continue
			}
			if init, ok := branch.Init.(*ast.AssignStmt); ok && len(init.Rhs) == 1 {
				if c, ok := init.Rhs[0].(*ast.CallExpr); ok && callName(c.Fun) == "utilfeature.DefaultMutableFeatureGate.SetFromMap" {
					if len(c.Args) != 1 || a.text(c.Args[0]) != "featureGates" || branch.Else != nil {
						return nil, fmt.Errorf("unrecognized feature gate initialization")
					}
					a.replace(stmt, "")
					continue
				}
			}
		}
		startup.List = append(startup.List, stmt)
	}
	if err := a.startup(startup); err != nil {
		return nil, err
	}
	// Rewrite only calls in the entrypoint's startup region; helpers and upstream
	// controller packages are not subject to generic fatal/context substitution.
	var callError error
	ast.Inspect(startup, func(n ast.Node) bool {
		if _, ok := n.(*ast.GoStmt); ok {
			return false
		} // replaced as a unit
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 {
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && callName(call.Fun) == "controller.NewProvisionController" {
				if len(assign.Lhs) != 1 || a.text(assign.Lhs[0]) != "provisionController" || assign.Tok != token.ASSIGN || len(call.Args) != 5 {
					callError = fmt.Errorf("unexpected provisioner constructor assignment")
					return false
				}
				a.insert(assign.Pos(), "var shutdownProvisionerEvents func()\n")
				a.insert(assign.Lhs[0].End(), ", shutdownProvisionerEvents, err")
				a.replace(call.Fun, "controller.NewProvisionControllerAIO")
				a.insert(assign.End(), "\nif err != nil { return csistartup.Error(ctx, \"initialize provisioner\", err) }\nscope.Defer(shutdownProvisionerEvents)\n")
			}
		}
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 {
			if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && callName(call.Fun) == "controller.NewCSISnapshotSideCarController" {
				if sidecar != "snapshotter" || len(assign.Lhs) != 1 || a.text(assign.Lhs[0]) != "ctrl" || assign.Tok != token.DEFINE || len(call.Args) != 19 {
					callError = fmt.Errorf("unexpected snapshot constructor assignment")
					return false
				}
				a.insert(assign.Lhs[0].End(), ", shutdownSnapshotEvents, err")
				a.replace(call.Fun, "controller.NewCSISnapshotSideCarControllerAIO")
				a.insert(call.Lparen+1, "ctx, ")
				a.insert(assign.End(), "\nif err != nil { return csistartup.Error(ctx, \"initialize snapshotter\", err) }\nscope.Defer(shutdownSnapshotEvents)\n")
			}
		}
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callName(c.Fun) {
		case "snapshotscheme.AddToScheme":
			if sidecar != "snapshotter" || tokens(a.text(c)) != tokens("snapshotscheme.AddToScheme(scheme.Scheme)") {
				callError = fmt.Errorf("unexpected snapshot scheme registration")
				return false
			}
			a.replace(c, "")
		case "kubernetes.NewForConfig", "clientset.NewForConfig":
			if sidecar == "snapshotter" {
				if len(c.Args) != 1 || (a.text(c.Args[0]) != "config" && a.text(c.Args[0]) != "coreConfig") {
					callError = fmt.Errorf("unexpected snapshot API client arguments")
					return false
				}
				a.replace(c.Args[0], "csistartup.OwnConfig(ctx, "+a.text(c.Args[0])+")")
			}
		case "connection.ExitOnConnectionLoss":
			if len(c.Args) != 0 {
				callError = fmt.Errorf("unexpected connection loss arguments")
				return false
			}
			a.replace(c, "scope.ConnectionLost")
		case "rpc.ProbeForever", "csirpc.ProbeForever", "ctrl.Probe":
			if len(c.Args) != 3 {
				callError = fmt.Errorf("unexpected probe arguments")
				return false
			}
			a.replace(c.Fun, "csistartup.ProbeForever")
		case "ctrl.Connect":
			if len(c.Args) != 3 {
				callError = fmt.Errorf("unexpected connection arguments")
				return false
			}
			a.replace(c, "connection.Connect("+a.text(c.Args[0])+", "+a.text(c.Args[1])+", "+a.text(c.Args[2])+", connection.OnConnectionLoss(scope.ConnectionLost))")
		case "ctrl.GetDriverName", "ctrl.GetDriverCapabilities", "ctrl.GetNodeInfo":
			if len(c.Args) != 2 {
				callError = fmt.Errorf("unexpected startup RPC arguments")
				return false
			}
			a.replace(c, "csistartup."+strings.TrimPrefix(callName(c.Fun), "ctrl.")+"(ctx, "+a.text(c.Args[0])+", "+a.text(c.Args[1])+")")
		case "owner.Lookup":
			if len(c.Args) != 5 {
				callError = fmt.Errorf("unexpected owner lookup arguments")
				return false
			}
			a.replace(c.Fun, "owner.LookupAIO")
			a.insert(c.Lparen+1, "ctx, ")
		case "csi.New":
			if tokens(a.text(c)) != tokens("csi.New(ctx, standardflags.Configuration.CSIAddress, *timeout, metricsManager)") {
				callError = fmt.Errorf("unexpected resizer client arguments")
				return false
			}
			a.replace(c, "csi.NewAIO(ctx, standardflags.Configuration.CSIAddress, *timeout, metricsManager, scope.ConnectionLost)")
		case "getDriverName":
			if tokens(a.text(c)) != tokens("getDriverName(csiClient, *timeout)") {
				callError = fmt.Errorf("unexpected resizer driver name arguments")
				return false
			}
			a.replace(c, "csistartup.DriverName(ctx, csiClient.GetDriverName, *timeout)")
		case "connection.SetMaxGRPCLogLength":
			callError = fmt.Errorf("unexpected nested gRPC logging configuration: %s", a.text(c))
		case "context.Background", "context.TODO":
			callError = fmt.Errorf("unexpected detached startup context: %s", a.text(c))
		}
		return true
	})
	if callError != nil {
		return nil, callError
	}
	// Cleanup follows worker drain, including replacement connections for migrated drivers.
	conn := map[string]string{"attacher": "csiConn.Close()", "provisioner": "grpcClient.Close()", "snapshotter": "csiConn.Close()", "resizer": "csiClient.CloseConnection()"}[sidecar]
	variable := strings.Split(conn, ".")[0]
	for i, stmt := range startup.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) == 0 || a.text(assign.Lhs[0]) != variable {
			continue
		}
		if i+1 >= len(startup.List) {
			return nil, fmt.Errorf("missing connection error check")
		}
		check, ok := startup.List[i+1].(*ast.IfStmt)
		if !ok || tokens(a.text(check.Cond)) != tokens("err != nil") {
			return nil, fmt.Errorf("unexpected connection error check")
		}
		a.insert(check.End(), "\nscope.Defer(func() { "+conn+" })\n")
	}
	// The provisioner historically writes the shared flag object at startup.
	// Give that entrypoint a private value before its environment fallback.
	if sidecar == "provisioner" {
		a.insert(entry.Body.Lbrace+1, "\nprovisionerOptionsConfig := standardflags.Configuration\n")
		ast.Inspect(startup, func(n ast.Node) bool {
			s, ok := n.(*ast.SelectorExpr)
			if ok && callName(s) == "standardflags.Configuration.KubeConfig" {
				a.replace(s, "provisionerOptionsConfig.KubeConfig")
				return false
			}
			return true
		})
	}
	for _, imp := range file.Imports {
		if imp.Path.Value == `"github.com/kubernetes-csi/csi-lib-utils/leaderelection"` {
			a.replace(imp.Path, `"github.com/kubernetes-csi/csi-sidecars/pkg/leaderelection"`)
		}
	}
	a.insert(file.Name.End(), `
import (
 aioruntime "github.com/kubernetes-csi/csi-sidecars/pkg/runtime"
 aioConfiguration "github.com/kubernetes-csi/csi-sidecars/cmd/csi-sidecars/config"
 "github.com/kubernetes-csi/csi-sidecars/pkg/csistartup"
 "github.com/kubernetes-csi/csi-lib-utils/connection"
 "github.com/kubernetes-csi/csi-lib-utils/standardflags"
 "fmt"
 "sync"
)
`)
	output, err := a.apply()
	if err != nil {
		return nil, err
	}
	return cleanImports(output)
}

func (a *adapter) apply() ([]byte, error) {
	sort.SliceStable(a.edits, func(i, j int) bool { return a.edits[i].start < a.edits[j].start })
	var out bytes.Buffer
	offset := 0
	for _, e := range a.edits {
		if e.start < offset {
			return nil, fmt.Errorf("overlapping lifecycle adaptations at byte %d", e.start)
		}
		out.Write(a.source[offset:e.start])
		out.WriteString(e.text)
		offset = e.end
	}
	out.Write(a.source[offset:])
	return out.Bytes(), nil
}

func cleanImports(source []byte) ([]byte, error) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "adapted.go", source, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok {
				used[id.Name] = true
			}
		}
		return true
	})
	seen := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		specs := gen.Specs[:0]
		for _, spec := range gen.Specs {
			imp := spec.(*ast.ImportSpec)
			path, _ := strconv.Unquote(imp.Path.Value)
			name := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			// These versioned packages have non-path-base default package names.
			if path == "k8s.io/klog/v2" {
				name = "klog"
			}
			key := path + ":" + name
			if !seen[key] && (name == "_" || used[name]) {
				specs = append(specs, spec)
				seen[key] = true
			}
		}
		gen.Specs = specs
	}
	var out bytes.Buffer
	if err := format.Node(&out, set, file); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: lifecycle-adapter CONTROLLER FILE")
		os.Exit(2)
	}
	path := os.Args[2]
	if os.Args[1] == "--dependencies" {
		if err := generateDependencies(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if os.Args[1] == "--workers" {
		if err := generateWorkers(path); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	source, err := os.ReadFile(path)
	if err == nil {
		var result []byte
		result, err = adapt(source, os.Args[1])
		if err == nil {
			err = os.WriteFile(path, result, 0644)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		os.Exit(1)
	}
}
