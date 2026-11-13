package proxytest

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRecorderHoldsItsPortOnBothFamilies pins the fix for TXE-7276: a recorder's port is
// held on both loopback families, so no other recorder (or anything else) can take the
// same port number on the other family and answer a "localhost:port" URL meant for it.
// Shard 6 of #252 failed when env-https6 ([::1]) was given the port of config
// (127.0.0.1), and "http://localhost:<config port>" reached env-https6.
func TestRecorderHoldsItsPortOnBothFamilies(t *testing.T) {
	if !loopbackAvailable("tcp6", "::1") || !loopbackAvailable("tcp4", "127.0.0.1") {
		t.Skip("this host lacks a loopback address on one family")
	}
	for _, tc := range []struct {
		network, other, otherHost string
		recorder                  func(t *testing.T) *Recorder
	}{
		{"tcp4", "tcp6", "::1", func(t *testing.T) *Recorder { return NewHTTPRecorder(t, "v4", "tcp4") }},
		{"tcp6", "tcp4", "127.0.0.1", func(t *testing.T) *Recorder { return NewHTTPRecorder(t, "v6", "tcp6") }},
		{"https", "tcp6", "::1", func(t *testing.T) *Recorder { return NewHTTPSRecorder(t, "tls") }},
		{"socks5", "tcp6", "::1", func(t *testing.T) *Recorder { return NewSOCKS5Recorder(t, "socks") }},
	} {
		t.Run(tc.network, func(t *testing.T) {
			r := tc.recorder(t)
			squatter, err := net.Listen(tc.other, net.JoinHostPort(tc.otherHost, r.Port()))
			if err == nil {
				squatter.Close()
				t.Fatalf("port %s is free on %s, so another recorder can take it there", r.Port(), tc.other)
			}
		})
	}
}

// TestLocalhostReachesTheRecorderItNames sends a CONNECT to "localhost:<port>" for a
// recorder of each family and checks that recorder records it, whichever family
// localhost resolves to first.
func TestLocalhostReachesTheRecorderItNames(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			host := "127.0.0.1"
			if network == "tcp6" {
				host = "::1"
			}
			if !loopbackAvailable(network, host) {
				t.Skip("no loopback on " + network)
			}
			r := NewHTTPRecorder(t, network, network)
			for _, dialNetwork := range []string{"tcp4", "tcp6"} {
				dialHost := map[string]string{"tcp4": "127.0.0.1", "tcp6": "::1"}[dialNetwork]
				if !loopbackAvailable(dialNetwork, dialHost) {
					continue
				}
				r.Reset()
				conn, err := net.DialTimeout(dialNetwork, net.JoinHostPort(dialHost, r.Port()), 3*time.Second)
				if err != nil {
					t.Fatalf("dial %s: %v", dialNetwork, err)
				}
				fmt.Fprintf(conn, "CONNECT target.test:443 HTTP/1.1\r\nHost: target.test:443\r\n\r\n")
				_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				line, err := bufio.NewReader(conn).ReadString('\n')
				conn.Close()
				if err != nil || !strings.Contains(line, "200") {
					t.Fatalf("via %s: got %q, %v", dialNetwork, line, err)
				}
				if seen := r.Seen(); len(seen) != 1 || seen[0].Target != "target.test:443" {
					t.Fatalf("via %s: recorder saw %+v", dialNetwork, seen)
				}
			}
		})
	}
}

// TestIsolateEnvClearsEveryProxyVariable checks IsolateEnv against EnvNames.
func TestIsolateEnvClearsEveryProxyVariable(t *testing.T) {
	for _, name := range EnvNames {
		t.Setenv(name, "http://127.0.0.1:1")
	}
	IsolateEnv(t)
	for _, name := range EnvNames {
		if value := os.Getenv(name); value != "" {
			t.Errorf("%s=%q after IsolateEnv", name, value)
		}
	}
}

// isolatingCalls are the proxytest functions that clear the proxy environment.
var isolatingCalls = map[string]bool{
	"IsolateEnv": true, "SetEnv": true, "NewSet": true,
	"NewHTTPRecorder": true, "NewHTTPSRecorder": true, "NewSOCKS5Recorder": true,
}

// TestProxyEnvIsIsolated is the class check of TXE-7276: a test function in core,
// transports, framework or plugins that sets a proxy variable by hand (t.Setenv or
// os.Setenv with HTTP_PROXY, HTTPS_PROXY, NO_PROXY, ALL_PROXY in either case) must
// first clear the rest with proxytest.IsolateEnv (or call SetEnv, NewSet or a recorder
// constructor, which do). Otherwise a variable the runner sets and the test does not
// (https_proxy, which wins over HTTPS_PROXY in httpproxy, or NO_PROXY) changes the
// result. Tests that use the recorders are isolated by construction.
func TestProxyEnvIsIsolated(t *testing.T) {
	root := repoRoot(t)
	proxyVars := map[string]bool{}
	for _, name := range EnvNames {
		if strings.HasSuffix(strings.ToUpper(name), "_PROXY") {
			proxyVars[name] = true
		}
	}
	var offenders []string
	scanned := 0
	for _, dir := range []string{"core", "transports", "framework", "plugins"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			scanned++
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				setsProxy, isolates, ownList := scanProxyEnv(fn.Body, proxyVars)
				if setsProxy.IsValid() && !isolates {
					offenders = append(offenders, fmt.Sprintf("%s:%d %s sets a proxy variable without proxytest.IsolateEnv", rel, fset.Position(setsProxy).Line, fn.Name.Name))
				}
				if ownList.IsValid() {
					offenders = append(offenders, fmt.Sprintf("%s:%d %s keeps its own list of proxy variables; proxytest.IsolateEnv clears them all", rel, fset.Position(ownList).Line, fn.Name.Name))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d test files under %s: the walk is broken", scanned, root)
	}
	if len(offenders) > 0 {
		t.Fatalf("call proxytest.IsolateEnv(t) before setting proxy variables by hand:\n%s", strings.Join(offenders, "\n"))
	}
}

// scanProxyEnv reports the first Setenv of a proxy variable in body (token.NoPos when
// none), whether body calls one of isolatingCalls on the proxytest package (or
// unqualified, inside package proxytest), and the first slice literal naming two or more
// proxy variables: a hand-kept clearing list, which drifts from EnvNames (the lists
// IsolateEnv replaced each missed ALL_PROXY or the lowercase forms).
func scanProxyEnv(body *ast.BlockStmt, proxyVars map[string]bool) (token.Pos, bool, token.Pos) {
	setsProxy, ownList := token.NoPos, token.NoPos
	isolates := false
	ast.Inspect(body, func(n ast.Node) bool {
		if list, ok := n.(*ast.CompositeLit); ok && !ownList.IsValid() {
			named := 0
			for _, elt := range list.Elts {
				if lit, ok := elt.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if value, err := strconv.Unquote(lit.Value); err == nil && proxyVars[value] {
						named++
					}
				}
			}
			if named >= 2 {
				ownList = list.Pos()
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name, qualifier string
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fun.Sel.Name
			if ident, ok := fun.X.(*ast.Ident); ok {
				qualifier = ident.Name
			}
		case *ast.Ident:
			name = fun.Name
		}
		if isolatingCalls[name] && (qualifier == "proxytest" || qualifier == "") {
			isolates = true
		}
		if name == "Setenv" && len(call.Args) > 0 && !setsProxy.IsValid() {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil && proxyVars[value] {
					setsProxy = call.Pos()
				}
			}
		}
		return true
	})
	return setsProxy, isolates, ownList
}

// repoRoot is the directory holding go.work above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.work not found above the test directory")
		}
		dir = parent
	}
}

func loopbackAvailable(network, host string) bool {
	l, err := net.Listen(network, net.JoinHostPort(host, "0"))
	if err != nil {
		return false
	}
	l.Close()
	return true
}
