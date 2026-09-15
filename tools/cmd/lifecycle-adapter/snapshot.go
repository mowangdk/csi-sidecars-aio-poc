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

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

const snapshotController = "pkg/snapshotter/pkg/sidecar-controller"

func cloneSnapshotConstructor(source []byte) ([]byte, error) {
	source, err := methodSource(source, "", "NewCSISnapshotSideCarController")
	if err != nil {
		return nil, err
	}
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "snapshot.go", source, 0)
	if err != nil {
		return nil, err
	}
	a := &adapter{source: source, set: set}
	a.insert(file.Name.End(), `
import (
 "context"
 k8sruntime "k8s.io/apimachinery/pkg/runtime"
 snapshotscheme "github.com/kubernetes-csi/csi-sidecars/pkg/snapshotter/client/clientset/versioned/scheme"
)
`)
	var broadcaster, recorder, handler, returns int
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if len(fn.Type.Params.List) != 19 || tokens(a.text(fn.Type.Results)) != tokens("*csiSnapshotSideCarController") {
			return nil, fmt.Errorf("unknown snapshot constructor signature")
		}
		a.replace(fn.Name, "NewCSISnapshotSideCarControllerAIO")
		a.insert(fn.Type.Params.Opening+1, "ctx context.Context, ")
		a.replace(fn.Type.Results, "(*csiSnapshotSideCarController, func(), error)")
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				switch callName(call.Fun) {
				case "record.NewBroadcaster":
					if len(call.Args) != 0 {
						err = fmt.Errorf("unknown snapshot broadcaster")
						return false
					}
					a.insert(call.Lparen+1, "record.WithContext(ctx)")
					broadcaster++
				case "broadcaster.NewRecorder":
					if len(call.Args) != 2 || a.text(call.Args[0]) != "scheme.Scheme" {
						err = fmt.Errorf("unknown snapshot recorder")
						return false
					}
					a.replace(call.Args[0], "eventScheme")
					recorder++
				case "NewCSIHandler":
					if len(call.Args) != 7 {
						err = fmt.Errorf("unknown snapshot handler arguments")
						return false
					}
					a.replace(call.Fun, "NewCSIHandlerAIO")
					a.insert(call.Lparen+1, "ctx, ")
					handler++
				}
			}
			if ret, ok := n.(*ast.ReturnStmt); ok {
				if tokens(a.text(ret)) != tokens("return ctrl") {
					err = fmt.Errorf("unknown snapshot constructor return")
					return false
				}
				a.replace(ret, "return ctrl, broadcaster.Shutdown, nil")
				returns++
			}
			return true
		})
		a.insert(fn.Body.Lbrace+1, `
 eventScheme := k8sruntime.NewScheme()
 if err := v1.AddToScheme(eventScheme); err != nil { return nil, nil, err }
 if err := snapshotscheme.AddToScheme(eventScheme); err != nil { return nil, nil, err }
`)
	}
	if err != nil {
		return nil, err
	}
	if broadcaster != 1 || recorder != 1 || handler != 1 || returns != 1 {
		return nil, fmt.Errorf("snapshot constructor lifecycle changed")
	}
	out, err := a.apply()
	if err != nil {
		return nil, err
	}
	return checkedSnapshotLifecycle(out)
}

var snapshotOperations = []string{"CreateSnapshot", "DeleteSnapshot", "GetSnapshotStatus", "CreateGroupSnapshot", "DeleteGroupSnapshot", "GetGroupSnapshotStatus"}

func cloneSnapshotHandler(source []byte) ([]byte, error) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "handler.go", source, 0)
	if err != nil {
		return nil, err
	}
	a := &adapter{source: source, set: set}
	var wrapper string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "NewCSIHandler" {
			continue
		}
		if len(fn.Type.Params.List) != 7 || tokens(a.text(fn.Type.Results)) != tokens("Handler") || len(fn.Body.List) != 1 {
			return nil, fmt.Errorf("unknown snapshot handler constructor")
		}
		ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return nil, fmt.Errorf("unknown handler return")
		}
		ptr, ok := ret.Results[0].(*ast.UnaryExpr)
		if !ok || ptr.Op != token.AND {
			return nil, fmt.Errorf("unknown handler allocation")
		}
		literal, ok := ptr.X.(*ast.CompositeLit)
		if !ok || callName(literal.Type) != "csiHandler" {
			return nil, fmt.Errorf("unknown handler type")
		}
		var args []string
		for _, param := range fn.Type.Params.List {
			if len(param.Names) != 1 {
				return nil, fmt.Errorf("unknown handler parameters")
			}
			args = append(args, param.Names[0].Name)
		}
		params := strings.TrimSpace(a.text(fn.Type.Params))
		wrapper = "func NewCSIHandlerAIO(ctx context.Context, " + params[1:] + " Handler {\nreturn &aioCSIHandler{csiHandler: NewCSIHandler(" + strings.Join(args, ", ") + ").(*csiHandler), ctx: ctx}\n}\n"
	}
	if wrapper == "" {
		return nil, fmt.Errorf("missing snapshot handler constructor")
	}
	source, err = methodSource(source, "csiHandler", snapshotOperations...)
	if err != nil {
		return nil, err
	}
	set = token.NewFileSet()
	file, err = parser.ParseFile(set, "operations.go", source, 0)
	if err != nil {
		return nil, err
	}
	a = &adapter{source: source, set: set}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		a.replace(fn.Recv.List[0].Type, "*aioCSIHandler")
		if len(fn.Body.List) < 2 || tokens(a.text(fn.Body.List[0])) != tokens("ctx, cancel := context.WithTimeout(context.Background(), handler.timeout)") || tokens(a.text(fn.Body.List[1])) != tokens("defer cancel()") {
			return nil, fmt.Errorf("unknown snapshot RPC lifecycle in %s", fn.Name.Name)
		}
		a.replace(fn.Body.List[0], "ctx, cancel := context.WithTimeout(handler.ctx, handler.timeout)")
	}
	out, err := a.apply()
	if err != nil {
		return nil, err
	}
	out = append(out, []byte("\ntype aioCSIHandler struct { *csiHandler; ctx context.Context }\n"+wrapper)...)
	return checkedSnapshotLifecycle(out)
}

// The recognized snapshot constructor and RPC methods are synchronous. Reject
// newly introduced lifecycle ownership instead of silently copying it into AIO.
// This checks the selected, adapted functions, not unrelated upstream helpers.
func checkedSnapshotLifecycle(source []byte) ([]byte, error) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "snapshot-lifecycle.go", source, 0)
	if err != nil {
		return nil, err
	}
	a := &adapter{source: source, set: set}
	ast.Inspect(file, func(n ast.Node) bool {
		if err != nil {
			return false
		}
		switch node := n.(type) {
		case *ast.GoStmt:
			err = fmt.Errorf("unowned snapshot goroutine: %s", a.text(node))
		case *ast.CallExpr:
			if terminal(node) {
				err = fmt.Errorf("unowned snapshot process exit: %s", a.text(node))
			}
			switch callName(node.Fun) {
			case "context.Background", "context.TODO", "context.WithoutCancel", "signal.Notify", "signal.NotifyContext", "signals.SetupSignalHandler", "klog.Fatalln", "runtime.Goexit":
				err = fmt.Errorf("unowned snapshot lifecycle call: %s", a.text(node))
			}
		}
		return err == nil
	})
	if err != nil {
		return nil, err
	}
	return cleanImports(source)
}

func snapshotOutputs(root string) (map[string][]byte, error) {
	outputs := map[string][]byte{}
	for name, adapt := range map[string]func([]byte) ([]byte, error){
		"snapshot_controller_base.go": cloneSnapshotConstructor,
		"csi_handler.go":              cloneSnapshotHandler,
	} {
		source, err := os.ReadFile(filepath.Join(root, snapshotController, name))
		if err != nil {
			return nil, err
		}
		out, err := adapt(source)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		outputs[filepath.Join(root, snapshotController, "aio_owned_"+name)] = out
	}
	return outputs, nil
}
