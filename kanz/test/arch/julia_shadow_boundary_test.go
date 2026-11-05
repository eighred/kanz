package arch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// #1413: bounded syntax/import checks for the trusted local shadow worker.
// This is not whole-program dataflow or an OS sandbox proof. Runtime tests own
// subprocess failure/queue behavior and mutation isolation of captured inputs.
func juliaShadowBoundary(files map[string]string) error {
	compact := func(s string) string { return regexp.MustCompile(`\s+`).ReplaceAllString(s, "") }
	for name, source := range files {
		if !strings.HasPrefix(name, "worker/") && !strings.HasPrefix(name, "process/") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(path, modulePath+"/") && !strings.HasPrefix(path, modulePath+"/kanz-schemas-go/") && path != modulePath+"/internal/risk/pricing/curve" && path != modulePath+"/services/risk-engine/internal/shadowprocess" {
				return fmt.Errorf("shadow dependency grants authority: %s", path)
			}
			if path == "syscall" || strings.HasPrefix(path, "net") {
				return fmt.Errorf("shadow worker cannot add direct transport/syscall authority")
			}
		}
		var violation error
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if node.Sel.Name == "Publish" || node.Sel.Name == "Put" || node.Sel.Name == "OnCalibrated" || node.Sel.Name == "NewStore" || node.Sel.Name == "Environ" {
					violation = fmt.Errorf("shadow cannot call authoritative sink or inherit environment: %s", node.Sel.Name)
				}
			case *ast.CallExpr:
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") {
					if len(node.Args) != 1 {
						violation = fmt.Errorf("invalid environment read")
						break
					}
					lit, ok := node.Args[0].(*ast.BasicLit)
					if !ok || lit.Value != `"SystemRoot"` {
						violation = fmt.Errorf("child may inherit only SystemRoot")
					}
				}
			}
			return true
		})
		if violation != nil {
			return violation
		}
	}
	w := compact(files["worker/worker.go"])
	for _, required := range []string{`case w.queue <- c: default:`, `runProcess(ctx,c,input,[]string{"--startup-file=no","--history-file=no","--compiled-modules=existing","--pkgimages=existing","--project="+c.Project,c.Script})`, `"JULIA_LOAD_PATH=@"+string(os.PathListSeparator)+"@stdlib"`, `return shadowprocess.Run(ctx,shadowprocess.Config{Executable:c.Executable,Directory:c.Project,Args:args,Timeout:c.Timeout,Environment:[]string{"JULIA_PKG_OFFLINE=true","JULIA_LOAD_PATH=@"+string(os.PathListSeparator)+"@stdlib","JULIA_DEPOT_PATH="+c.Depot,"JULIA_NUM_THREADS=1","OPENBLAS_NUM_THREADS=1"}},input)`} {
		if !strings.Contains(w, compact(required)) {
			return fmt.Errorf("missing bounded queue/process seam: %s", required)
		}
	}
	process := compact(files["process/process.go"])
	for _, required := range []string{`const MaxBytes=1<<20`, `exec.CommandContext(ctx,c.Executable,c.Args...)`, `cmd.Env=append([]string{},c.Environment...)`, `cmd.WaitDelay=time.Second`, `context.WithTimeout(ctx,c.Timeout)`, `out:=&boundedBuffer{limit:MaxBytes,cancel:cancel}`, `stderr:=&boundedBuffer{limit:32<<10,cancel:cancel}`, `if len(p)>b.limit-b.buffer.Len(){b.exceeded=true`, `b.cancel()`, `if len(input)>MaxBytes`, `cmd.Dir=scratch`} {
		if !strings.Contains(process, compact(required)) {
			return fmt.Errorf("missing shared subprocess bound: %s", required)
		}
	}
	c := compact(files["calibrate"])
	put := strings.Index(c, `cal.Store.Put(currency,asOf,stored)`)
	observe := strings.Index(c, `cal.OnCaptured(CalibrationCapture{`)
	if put < 0 || observe < put || !strings.Contains(c, `Strip:strip.clone()`) || !strings.Contains(c, `Tenors:stored.Tenors()`) || !strings.Contains(c, `Zeros:stored.Zeros()`) {
		return fmt.Errorf("capture must follow authoritative commit with detached inputs/results")
	}
	main := compact(files["main"])
	if !strings.Contains(main, `shadow,err:=newJuliaShadow(ctx,cfg,obs.Registry)`) || !strings.Contains(compact(files["builder"]), `shadow,err:=calibrationshadow.New(ctx,cfg.JuliaShadow,`) {
		return fmt.Errorf("composition must construct the configured shadow worker")
	}
	if !strings.Contains(main, `ifshadow!=nil{cal.OnCaptured=shadow.Enqueue}`) || strings.Count(main, "OnCaptured") != 1 {
		return fmt.Errorf("composition must connect capture only to a configured shadow queue")
	}
	r := compact(files["ratesource"])
	if strings.Count(r, `s.quotes.snapshot()`) != 1 || strings.Contains(r, `s.quotes.Latest(`) || !strings.Contains(r, `ev,ok:=observations[in.InstrumentID]`) {
		return fmt.Errorf("rate strip must use one cache capture, without per-input rereads")
	}
	return nil
}

func juliaBoundarySources(t *testing.T) map[string]string {
	t.Helper()
	root := moduleRoot(t)
	files := map[string]string{}
	paths := map[string]string{"main": "services/risk-engine/cmd/risk-engine/main.go", "builder": "services/risk-engine/cmd/risk-engine/julia_shadow.go", "calibrate": "internal/risk/pricing/curve/calibrate.go", "ratesource": "internal/risk/pricing/livequote/ratesource.go"}
	processRoot := "services/risk-engine/internal/shadowprocess"
	processEntries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(processRoot)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range processEntries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			paths["process/"+e.Name()] = processRoot + "/" + e.Name()
		}
	}
	worker := "services/risk-engine/internal/calibrationshadow"
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(worker)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			paths["worker/"+e.Name()] = worker + "/" + e.Name()
		}
	}
	for key, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		files[key] = string(data)
	}
	return files
}

func TestJuliaShadowBoundary(t *testing.T) {
	if err := juliaShadowBoundary(juliaBoundarySources(t)); err != nil {
		t.Fatal(err)
	}
}
func TestJuliaShadowBoundaryMutations(t *testing.T) {
	base := juliaBoundarySources(t)
	if err := juliaShadowBoundary(base); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, file, old, new string }{
		{"app-env", "process/process.go", "cmd.Env = append([]string{}, c.Environment...)", "cmd.Env = os.Environ()"},
		{"secret-read", "process/process.go", `os.Getenv("SystemRoot")`, `os.Getenv("DATABASE_URL")`},
		{"authority", "worker/worker.go", "package calibrationshadow", "package calibrationshadow\nimport _ \"github.com/eighred/kanz/pkg/bus\""},
		{"install-result", "worker/worker.go", "func finite(", "func forbidden(s interface{Put()}) { s.Put() }; func finite("},
		{"capture-alias", "calibrate", "Strip: strip.clone()", "Strip: strip"},
		{"skip-commit", "calibrate", "cal.Store.Put(currency, asOf, stored)", "// removed authoritative installation"},
		{"second-read", "ratesource", "observations[in.InstrumentID]", "s.quotes.Latest(in.InstrumentID)"},
		{"wrong-observer", "main", "shadow.Enqueue", "shadow.Close"},
		{"disconnected-builder", "builder", "calibrationshadow.New", "calibrationshadow.Other"},
		{"unconditional-observer", "main", "if shadow != nil {", "if true {"},
		{"shell-command", "process/process.go", "exec.CommandContext(ctx, c.Executable, c.Args...)", "exec.CommandContext(ctx, \"sh\", c.Args...)"},
		{"blocking-queue", "worker/worker.go", "case w.queue <- c:\n\tdefault:", "case w.queue <- c:"},
		{"automatic-precompile", "worker/worker.go", "--compiled-modules=existing", "--compiled-modules=yes"},
		{"automatic-pkgimages", "worker/worker.go", "--pkgimages=existing", "--pkgimages=yes"},
		{"global-package-fallback", "worker/worker.go", "JULIA_LOAD_PATH=@", "JULIA_LOAD_PATH=@v#.#"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range base {
				files[k] = v
			}
			files[tc.file] = strings.Replace(files[tc.file], tc.old, tc.new, 1)
			if files[tc.file] == base[tc.file] {
				t.Fatal("mutation missed")
			}
			if juliaShadowBoundary(files) == nil {
				t.Fatal("mutation escaped")
			}
		})
	}
}
