package arch

import (
	"bytes"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const rustOMSPath = "services/oms-fold-shadow"

var rustOMSSources = []string{"build.rs", "src/main.rs", "src/lib.rs"}

const rustOMSCargo = `[package]
name = "kanz-oms-fold-shadow"
version = "0.1.0"
edition = "2021"
rust-version = "1.90"
publish = false

[dependencies]
kanz-decimal-compat = { path = "../.." }
prost = "=0.14.1"
prost-types = "=0.14.1"
num-bigint = "=0.4.6"
num-rational = "=0.4.2"
num-traits = "=0.2.19"

[build-dependencies]
prost-build = "=0.14.1"
protoc-bin-vendored = "=3.2.0"
`
const rustOMSBuild = `fn main() -> Result<(), Box<dyn std::error::Error>> {
    let root = "../../../kanz-schemas/proto";
    println!("cargo:rerun-if-changed={root}");
    println!("cargo:rerun-if-env-changed=KANZ_BUILD_VERSION");
    let mut config = prost_build::Config::new();
    config.file_descriptor_set_path(
        std::path::PathBuf::from(std::env::var("OUT_DIR")?).join("oms-descriptors.bin"),
    );
    config.protoc_executable(protoc_bin_vendored::protoc_bin_path()?);
    config.extern_path(".common.v1", "::kanz_decimal_compat::common::v1");
    config.compile_protos(
        &[
            format!("{root}/oms/v1/fold_shadow.proto"),
            format!("{root}/envelope/v1/event_frame.proto"),
            format!("{root}/domain/v1/portfolio.proto"),
            format!("{root}/command/v1/command_outcome.proto"),
        ],
        &[root],
    )?;
    Ok(())
}
`
const rustOMSMain = `#![forbid(unsafe_code)]
use kanz_oms_fold_shadow::{
    evaluate, evaluate_terminal,
    proto::oms::v1::{FillFoldRequest, TerminalFoldRequest},
};
use prost::Message;
use std::io::{Read, Write};
const LIMIT: usize = 1 << 20;
fn run() -> Result<(), Box<dyn std::error::Error>> {
    let version = option_env!("KANZ_BUILD_VERSION")
        .filter(|v| !v.trim().is_empty())
        .ok_or("unstamped")?;
    let mut wire = Vec::new();
    std::io::stdin()
        .take((LIMIT + 1) as u64)
        .read_to_end(&mut wire)?;
    if wire.is_empty() || wire.len() > LIMIT {
        return Err("input bound".into());
    }
    // Prost drops unknown fields: refuse any non-canonical input instead of
    // silently dropping metadata from candidate state. Go sends deterministic wire.
    let args: Vec<String> = std::env::args().skip(1).collect();
    let output = match args.as_slice() {
        [] => {
            let request = FillFoldRequest::decode(wire.as_slice())?;
            if request.encode_to_vec() != wire {
                return Err("unsupported wire metadata".into());
            }
            evaluate(&request, version).encode_to_vec()
        }
        [mode] if mode == "--terminal" => {
            let request = TerminalFoldRequest::decode(wire.as_slice())?;
            if request.encode_to_vec() != wire {
                return Err("unsupported wire metadata".into());
            }
            evaluate_terminal(&request, version).encode_to_vec()
        }
        _ => return Err("unsupported operation mode".into()),
    };
    if output.len() > LIMIT {
        return Err("output bound".into());
    }
    std::io::stdout().lock().write_all(&output)?;
    Ok(())
}
fn main() {
    if run().is_err() {
        eprintln!("OMS fold shadow refused request");
        std::process::exit(1)
    }
}
`

func checkRustOMSFillShadow(root string) error {
	dir := filepath.Join(root, rustOMSPath)
	for file, want := range map[string]string{"Cargo.toml": rustOMSCargo, "build.rs": rustOMSBuild, "src/main.rs": rustOMSMain} {
		raw, e := os.ReadFile(filepath.Join(dir, file))
		if e != nil || rustMigrationCompact(string(raw)) != rustMigrationCompact(want) {
			return fmt.Errorf("OMS shadow unreviewed recipe %s", file)
		}
	}
	allowed := map[string]bool{"build.rs": true, "src/main.rs": true, "src/lib.rs": true}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("OMS shadow symlink")
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, ".proto") {
			return fmt.Errorf("parallel OMS schema")
		}
		if strings.HasSuffix(rel, ".rs") && !allowed[rel] {
			return fmt.Errorf("unreviewed OMS Rust module")
		}
		if d.Name() == "Cargo.toml" && rel != "Cargo.toml" {
			return fmt.Errorf("nested OMS crate")
		}
		return nil
	})
	if err != nil {
		return err
	}
	raw, e := os.ReadFile(filepath.Join(dir, "src/lib.rs"))
	if e != nil {
		return e
	}
	source, e := rustMigrationWithoutComments(string(raw))
	if e != nil {
		return e
	}
	if regexp.MustCompile(`\b(?:f32|f64|unsafe|extern|TcpStream|Command|File)\b|std::(?:net|fs|process)|struct\s+(?:Decimal|Money)\b`).MatchString(strings.ReplaceAll(source, "forbid(unsafe_code)", "")) {
		return fmt.Errorf("OMS kernel has unauthorized runtime or financial model")
	}
	for _, required := range []string{"pub use kanz_decimal_compat::common;", "FillFoldStatus::UnsupportedInput", "FillFoldStatus::Rejected", "let mut candidate = state.clone();", "(-64..=64).contains", "OrderStatus::Filled", "OrderStatus::PartiallyFilled"} {
		if !strings.Contains(source, required) {
			return fmt.Errorf("OMS kernel missing invariant %s", required)
		}
	}
	return nil
}

var omsGoPaths = []string{"services/oms/internal/order/service.go", "services/oms/internal/order/recovery_commit.go", "services/oms/internal/order/fold_shadow.go", "services/oms/internal/foldshadow/worker.go", "services/oms/cmd/oms/fold_shadow.go", "services/oms/cmd/oms/main.go", "services/oms/internal/order/terminal_shadow.go", "services/oms/internal/foldshadow/terminal.go", "services/oms/internal/order/schedule_drive.go"}

func omsGoSources(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, p := range omsGoPaths {
		raw, e := os.ReadFile(filepath.Join(moduleRoot(t), p))
		if e != nil {
			t.Fatal(e)
		}
		m[p] = string(raw)
	}
	return m
}
func checkOMSGoBoundary(files map[string]string) error {
	parsed := map[string]string{}
	for path, source := range files {
		fset := token.NewFileSet()
		file, e := parser.ParseFile(fset, path, source, 0)
		if e != nil {
			return e
		}
		var b bytes.Buffer
		if e := format.Node(&b, fset, file); e != nil {
			return e
		}
		parsed[path] = rustMigrationCompact(b.String())
	}
	requirements := map[string][]string{
		"services/oms/internal/order/service.go":         {`next,aerr:=ApplyFill(st,fill,fill.GetExecutedAt().AsTime())ifaerr!=nil{deferobserveFillFold(ctx,s.fillFoldObserver,"live",ver,st,fill,fill.GetExecutedAt().AsTime(),next,aerr)`, `next,aerr:=ApplyFill(st,fill,fill.GetExecutedAt().AsTime())ifaerr!=nil{deferobserveFillFold(ctx,s.fillFoldObserver,"reconcile",ver,st,fill,fill.GetExecutedAt().AsTime(),next,aerr)`},
		"services/oms/internal/order/recovery_commit.go": {`p.applyRecoveredExecution(ctx,c,next,fill,now,version)`, `next,err:=ApplyFill(working,fill,now)iferr!=nil{deferobserveFillFold(ctx,p.fillFoldObserver,"recovery",expectedVersion,working,fill,now,next,err)`},
		"services/oms/internal/order/fold_shadow.go":     {`ifobserver==nil{return}`, `proto.Size(state)+proto.Size(fill)+proto.Size(candidate)>shadowprocess.MaxBytes-1024`, `c.Unsupported=trueobserver(c)return`, `proto.Clone(state)`, `proto.Clone(fill)`},
		"services/oms/internal/foldshadow/worker.go":     {`make(chancapturedWork,1)`, `evaluateTerminal(ctx,c,tenant,sourceRevision,*capture.terminal)`, `proto.Size(c.State)+proto.Size(c.Fill)+proto.Size(c.Candidate)>shadowprocess.MaxBytes-1024`, `c.Tenant!=tenant`, `unknown(c.State.ProtoReflect())`, `unknown(c.Fill.ProtoReflect())`, `r.ProducerVersion!=version`, `r.ExpectedVersion==nil`, `r.GetExpectedVersion()!=j.GetExpectedVersion()`, `!bytes.Equal(r.CaptureDigest,j.CaptureDigest)`, `!proto.Equal(c.Candidate,r.Candidate)`, `shadowprocess.Run(ctx,shadowprocess.Config{Executable:c.Executable,Timeout:c.Timeout},wire)`},
		"services/oms/cmd/oms/fold_shadow.go":            {`results.WithLabelValues(status).Inc()`, `ifworker!=nil{store.SetFillFoldObserver(worker.Submit)store.SetTerminalFoldObserver(worker.SubmitTerminal)}`, `worker.Close();pool.Close()`},
		"services/oms/cmd/oms/main.go":                   {`order.WithFillFoldObserver(fillFoldObserver(store))`, `order.WithTerminalFoldObserver(terminalFoldObserver(store))`, `returnpersistentStoresWithShadow(ctx,cfg,pool,logger,reg)`},
		"services/oms/internal/order/terminal_shadow.go": {`ifobserver==nil{return}`, `proto.Size(state)+proto.Size(candidate)+proto.Size(quantity)>shadowprocess.MaxBytes-1024`, `proto.Clone(state)`, `proto.Clone(quantity)`},
		"services/oms/internal/foldshadow/terminal.go":   {`proto.Size(c.State)+proto.Size(c.Candidate)+proto.Size(c.UnfilledQuantity)>shadowprocess.MaxBytes-1024`, `casew.queue<-capturedWork{terminal:&c}:`, `c.Tenant!=tenant`, `unknown(c.State.ProtoReflect())`, `unknown(r.ProtoReflect())`, `r.Operation!=j.Operation`, `r.ProducerVersion!=version`, `r.ExpectedVersion==nil`, `!bytes.Equal(r.CaptureDigest,j.CaptureDigest)`, `!proto.Equal(c.UnfilledQuantity,r.UnfilledQuantity)`, `!proto.Equal(c.Candidate,r.Candidate)`, `Args:[]string{"--terminal"}`},
		"services/oms/internal/order/schedule_drive.go":  {`next,pureUnfilled,err:=Expire(st,now)observeTerminalFold(ctx,s.terminalFoldObserver,"expire","schedule",ver,st,next,pureUnfilled,now,err)`},
	}
	requirements["services/oms/internal/order/service.go"] = append(requirements["services/oms/internal/order/service.go"],
		`next,cancelledQty,cerr:=Cancel(st,now)observeTerminalFold(ctx,s.terminalFoldObserver,"cancel","cancel",ver,st,next,cancelledQty,now,cerr)`,
		`withdrawn,cancelledQty,cerr:=Cancel(st,now)observeTerminalFold(ctx,s.terminalFoldObserver,"cancel","reconcile",ver,st,withdrawn,cancelledQty,now,cerr)`,
		`withdrawn,unfilledQty,eerr:=Expire(st,now)observeTerminalFold(ctx,s.terminalFoldObserver,"expire","reconcile",ver,st,withdrawn,unfilledQty,now,eerr)`)
	for path, markers := range requirements {
		for _, marker := range markers {
			if !strings.Contains(parsed[path], marker) {
				return fmt.Errorf("OMS shadow missing executable binding %s: %s", path, marker)
			}
		}
	}
	for _, site := range []struct{ path, call string }{
		{"services/oms/internal/order/service.go", `observeFillFold(ctx,s.fillFoldObserver,"live",ver,st,fill,fill.GetExecutedAt().AsTime(),next,aerr)`},
		{"services/oms/internal/order/service.go", `observeFillFold(ctx,s.fillFoldObserver,"reconcile",ver,st,fill,fill.GetExecutedAt().AsTime(),next,aerr)`},
		{"services/oms/internal/order/recovery_commit.go", `observeFillFold(ctx,p.fillFoldObserver,"recovery",expectedVersion,working,fill,now,next,err)`},
	} {
		if strings.Count(parsed[site.path], site.call) != 2 || !strings.Contains(parsed[site.path], "}"+site.call) {
			return fmt.Errorf("OMS shadow must observe refusal after blocking and success separately")
		}
	}
	// The worker accepts captures, never an authoritative store or publisher.
	worker := parsed["services/oms/internal/foldshadow/worker.go"] + parsed["services/oms/internal/foldshadow/terminal.go"]
	for _, forbidden := range []string{"order.Store", "pgx", "bus.New", ".Save(", ".Publish(", "os.Environ("} {
		if strings.Contains(worker, forbidden) {
			return fmt.Errorf("OMS shadow authority access %s", forbidden)
		}
	}
	return nil
}
func TestRustOMSFillShadowBoundary(t *testing.T) {
	if e := checkRustOMSFillShadow(filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")); e != nil {
		t.Fatal(e)
	}
	if e := checkOMSGoBoundary(omsGoSources(t)); e != nil {
		t.Fatal(e)
	}
}
func TestRustOMSFillShadowBoundaryMutations(t *testing.T) {
	baseline := omsGoSources(t)
	if e := checkOMSGoBoundary(baseline); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct{ path, from, to string }{
		{omsGoPaths[4], `results.WithLabelValues(status).Inc()`, `results.WithLabelValues(status)`},
		{omsGoPaths[0], `"live", ver`, `"live", 0`}, {omsGoPaths[0], `"reconcile", ver`, `"reconcile", 0`}, {omsGoPaths[1], `"recovery", expectedVersion`, `"recovery", 0`},
		{omsGoPaths[2], `proto.Size(state)+proto.Size(fill)+proto.Size(candidate) > shadowprocess.MaxBytes-1024`, `false`},
		{omsGoPaths[3], `r.ProducerVersion != version`, `false`}, {omsGoPaths[3], `c.Tenant != tenant`, `false`}, {omsGoPaths[3], `r.GetExpectedVersion() != j.GetExpectedVersion()`, `false`}, {omsGoPaths[3], `unknown(c.Fill.ProtoReflect())`, `false`}, {omsGoPaths[3], `!proto.Equal(c.Candidate, r.Candidate)`, `false`},
		{omsGoPaths[7], `r.Operation != j.Operation`, `false`}, {omsGoPaths[7], `!proto.Equal(c.UnfilledQuantity, r.UnfilledQuantity)`, `false`}, {omsGoPaths[7], `unknown(r.ProtoReflect())`, `false`}, {omsGoPaths[7], `[]string{"--terminal"}`, `[]string{"--other"}`},
		{omsGoPaths[6], `proto.Size(state)+proto.Size(candidate)+proto.Size(quantity) > shadowprocess.MaxBytes-1024`, `false`},
		{omsGoPaths[8], `"schedule", ver`, `"schedule", 0`}, {omsGoPaths[0], `"cancel", "cancel", ver`, `"cancel", "cancel", 0`},
	} {
		t.Run(tc.from, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range baseline {
				m[k] = v
			}
			if !strings.Contains(m[tc.path], tc.from) {
				t.Fatal("dead mutation", tc.from)
			}
			m[tc.path] = strings.Replace(m[tc.path], tc.from, tc.to, 1)
			if checkOMSGoBoundary(m) == nil {
				t.Fatal("mutation escaped")
			}
		})
	}
	root := filepath.Join(filepath.Dir(moduleRoot(t)), "kanz-rs")
	for _, tc := range []struct{ file, append string }{{"Cargo.toml", "\nreqwest=\"=0.1.0\"\n"}, {"src/lib.rs", "\nstruct Decimal { coefficient:i64 }\n"}, {"src/lib.rs", "\nfn forbidden()->f64{0.0}\n"}, {"src/main.rs", "\nfn extra(){}\n"}} {
		t.Run(tc.file+tc.append, func(t *testing.T) {
			dst := t.TempDir()
			for _, file := range append([]string{"Cargo.toml"}, rustOMSSources...) {
				data, e := os.ReadFile(filepath.Join(root, rustOMSPath, file))
				if e != nil {
					t.Fatal(e)
				}
				path := filepath.Join(dst, rustOMSPath, file)
				if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(path, data, 0600); e != nil {
					t.Fatal(e)
				}
			}
			path := filepath.Join(dst, rustOMSPath, tc.file)
			f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				t.Fatal(e)
			}
			_, e = f.WriteString(tc.append)
			_ = f.Close()
			if e != nil {
				t.Fatal(e)
			}
			if checkRustOMSFillShadow(dst) == nil {
				t.Fatal("Rust mutation escaped")
			}
		})
	}
}
