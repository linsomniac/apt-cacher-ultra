package observability

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// sweptEvent is a prose log message the flat-repository branch added and
// the snake_case event it became.
type sweptEvent struct{ old, event string }

// sweptEvents are the 43 renamed messages: the 19 the overview listed
// (ruling R-H3), then the 24 it did not (ruling R-H6).
var sweptEvents = []sweptEvent{
	{"suite onboarding failed", "suite_onboarding_failed"},
	{"suite signed-seen mark failed", "suite_signed_seen_mark_failed"},
	{"suite cap usage read failed", "suite_cap_usage_read_failed"},
	{"freshness: read anchor rows failed", "freshness_read_anchor_rows_failed"},
	{"freshness: read current snapshot failed", "freshness_read_current_snapshot_failed"},
	{"adoption: served anchor read failed; the flip barrier runs", "adoption_served_anchor_read_failed"},
	{"adoption: signed-anchor re-mark failed", "adoption_signed_anchor_remark_failed"},
	{"suite reaper: list suites failed", "suite_reaper_list_suites_failed"},
	{"suite reaper: read suite row failed", "suite_reaper_read_suite_row_failed"},
	{"suite reaper: delete metadata failed", "suite_reaper_delete_metadata_failed"},
	{"suite reaper: reap failed", "suite_reaper_reap_failed"},
	{"suite reaper: list unadopted anchors failed", "suite_reaper_list_unadopted_anchors_failed"},
	{"suite reaper: junk anchor reads failed", "suite_reaper_junk_anchor_reads_failed"},
	{"suite reaper: marker sweep failed", "suite_reaper_marker_sweep_failed"},
	{"admin: forget failed", "admin_forget_failed"},
	{"freshness: anchorless index over its size cap, dropped for the miss path", "freshness_anchorless_index_over_cap"},
	{"freshness: delete anchorless rows failed", "freshness_delete_anchorless_rows_failed"},
	{"suite reaper: list unadopted flat indexes failed", "suite_reaper_list_unadopted_flat_indexes_failed"},
	{"suite reaper: read signed evidence failed", "suite_reaper_read_signed_evidence_failed"},
	// ruling R-H6
	{"adoption: read prior snapshot failed", "adoption_read_prior_snapshot_failed"},
	{"adoption.require_signature = false: an empty keyring is allowed at startup; plain-text or malformed InRelease files are never adopted as signed metadata, and with an empty keyring signed suites cannot adopt until keys are installed or adoption.accept_any_signer is set", "adoption_require_signature_disabled"},
	{"displaced snapshot lookup failed", "displaced_snapshot_lookup_failed"},
	{"freshness: anchorless index failed its sniff, kept", "freshness_anchorless_index_sniff_failed_kept"},
	{"freshness: check cancelled", "freshness_check_cancelled"},
	{"freshness: check ended by shutdown", "freshness_check_ended_by_shutdown"},
	{"freshness: clamp adoption retry failed", "freshness_clamp_adoption_retry_failed"},
	{"freshness: clear adoption failure failed", "freshness_clear_adoption_failure_failed"},
	{"freshness: deferred never-adopted suite paced until its retry or the next refresh check", "freshness_deferred_never_adopted_suite_paced_until_retry"},
	{"freshness: never-adopted suite without a client-created anchor or flat index row, dormant", "freshness_never_adopted_suite_dormant_no_client_created_row"},
	{"freshness: never-adopted suite without recent interest, dormant", "freshness_never_adopted_suite_dormant_no_recent_interest"},
	{"freshness: no baseline anchor hash, skipping", "freshness_check_skipped_no_baseline_anchor_hash"},
	{"freshness: persist failure bump after a panic failed", "freshness_persist_failure_bump_after_panic_failed"},
	{"freshness: read suite row after a panic failed", "freshness_read_suite_row_after_panic_failed"},
	{"freshness: refused suite paced until its retry", "freshness_refused_suite_paced_until_retry"},
	{"freshness: repair deferred, no adoption slot", "freshness_repair_deferred_no_adoption_slot"},
	{"legacy trust audit: cached anchor skipped", "legacy_trust_audit_cached_anchor_skipped"},
	{"shutdown wait panicked", "shutdown_wait_panicked"},
	{"snapshot adopted_at lookup failed", "snapshot_adopted_at_lookup_failed"},
	{"suite cached anchor read failed", "suite_cached_anchor_read_failed"},
	{"suite file head read failed", "suite_file_head_read_failed"},
	{"suite reaper: suite busy, retried next run", "suite_reaper_suite_busy"},
	{"suite signed-seen backstop read failed", "suite_signed_seen_backstop_read_failed"},
	{"suite touch failed", "suite_touch_failed"},
}

// mainProseMessages are the prose messages main already logs. They are
// released (an operator's alert may match one), so they are the only
// messages that may stay prose (ruling R-H6). Renaming one removes it here.
var mainProseMessages = []string{
	"admin listener accepting",
	"admin shutdown returned error",
	"adoption.accept_any_signer = true: signatures are decoded but not cryptographically verified for unpinned suites; apt clients on the fleet remain the authoritative trust anchor (auditable per SPEC2 §5.1)",
	"adoption: commit failed",
	"adoption: hot-set computation failed (skipping prefetch)",
	"adoption: insert candidate failed",
	"adoption: natural key already adopted",
	"adoption: persist InRelease failed",
	"adoption: persist Release failed",
	"adoption: persist Release.gpg failed",
	"adoption: reusing orphaned candidate",
	"apt-cacher-ultra starting",
	"apt-cacher-ultra stopped",
	"blob existence check failed",
	"blob open failed",
	"blob stat failed",
	"blob written",
	"by-hash provenance lookup failed",
	"by-hash url_path insert failed",
	"cache close failed",
	"cache lookup failed",
	"cache write failed",
	"evict url_path failed",
	"fetch retry",
	"freshness: adopted suites stale beyond threshold",
	"freshness check failed",
	"freshness: host limiter acquire aborted",
	"freshness: InRelease 404 on first-ever check, falling back to detached",
	"freshness: list suites failed",
	"freshness: lookup InRelease url_path failed",
	"freshness: lookup Release.gpg url_path failed",
	"freshness: lookup Release url_path failed",
	"freshness: metadata anchor missing for adopted suite; reconstructing",
	"freshness: no cached InRelease url_path, skipping",
	"freshness: no cached Release.gpg url_path, skipping detached",
	"freshness: no cached Release url_path, skipping detached",
	"freshness: persist failure-bump failed",
	"freshness: persist success failed",
	"freshness: read suite row failed",
	"freshness: Release.gpg fetch failed; deferring adoption",
	"freshness: Release.gpg fetch returned non-200; deferring adoption",
	"freshness: re-seed metadata anchor failed",
	"freshness: snapshot lookup failed; falling back to url_path",
	"gc startup pass failed",
	"http force-close returned error",
	"http listener accepting",
	"https force-close returned error",
	"http shutdown returned error",
	"https listener accepting",
	"https shutdown returned error",
	"InRelease changed at upstream",
	"integrity: blob missing",
	"integrity: discard blob failed",
	"integrity: hash blob failed",
	"integrity: scan failed",
	"keyring: parse embedded failed; skipping",
	"keyring: parse failed; skipping file",
	"keyring: read dir failed",
	"keyring: stat dir failed",
	"listener exited unexpectedly",
	"package_hash lookup failed",
	"package_hash validate lookup failed",
	"phase2 adoption enabled",
	"post-recovery blob open failed",
	"post-recovery blob stat failed",
	"Release changed at upstream",
	"schema current",
	"schema migrated",
	"shutdown signal received",
	"singleflight coalesced",
	"snapshot_member lookup failed",
	"startup failed",
	"startup tmp sweep failed",
	"strict-mode coverage lookup failed",
	"suite_freshness lookup failed",
	"touch failed",
	"tunnel manager drain returned error",
	"unclassified fetch error",
}

// logSinks maps every function that logs one of its arguments as a
// record's message to that argument's index: slog's methods and this
// repository's forwarding helpers. A new helper that forwards a message
// parameter must be added here, so its callers' messages are checked.
var logSinks = map[string]int{
	"Debug": 0, "Info": 0, "Warn": 0, "Error": 0,
	"DebugContext": 1, "InfoContext": 1, "WarnContext": 1, "ErrorContext": 1,
	"Log": 2, "LogAttrs": 2,
	"warn":              1, // (*freshness.Reaper).warn
	"logPanic":          1, // freshness
	"reportPanic":       0, // (*freshness.Checker).reportPanic
	"logGoroutinePanic": 0, // (*handler.Handler).logGoroutinePanic
	"emitTlsMitmLog":    2, // cmd/apt-cacher-ultra: internal/proxy's events
	"LogFn":             1, // internal/proxy and tlsmitm event callbacks
	"logFn":             1,
}

// fieldMessages are the message arguments that are neither a literal, a
// constant nor a forwarding helper's parameter, each with the constants
// its value comes from. The list is checked against the code: every value
// the field is assigned in its file must be one of them (or a snake_case
// literal), so a new step fails until it is listed here.
var fieldMessages = map[string][]string{
	"internal/freshness/reaper.go h.step": {"stepReadRow", "stepDeleteMetadata", "stepReap"},
}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

type goFile struct {
	rel  string // relative to the repository root
	src  string
	file *ast.File
}

var guardFset = token.NewFileSet()

// parseTree parses every Go file under internal/ and cmd/ but this
// package's guard tests.
func parseTree(t *testing.T) []goFile {
	t.Helper()
	root := filepath.Join("..", "..")
	var files []goFile
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == "internal/observability/event_names_test.go" {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(guardFset, p, src, 0)
			if err != nil {
				return err
			}
			files = append(files, goFile{rel: rel, src: string(src), file: f})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// logMessages returns every log message of production code (internal/
// and cmd/, test files aside) with the first place it is logged: the
// message argument of each logSinks call, when it is a string literal or
// a string constant of its package; the constants fieldMessages lists for
// a field, and any snake_case literal the field's file assigns to it. Any
// other message argument is reported (t.Errorf) unless it is a parameter
// of a forwarding helper logSinks lists, whose own callers are checked; so
// is any other value assigned to a fieldMessages field.
func logMessages(t *testing.T) map[string]string {
	t.Helper()
	files := parseTree(t)
	consts := map[string]map[string]string{} // package directory -> name -> value
	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		dir := filepath.Dir(f.rel)
		if consts[dir] == nil {
			consts[dir] = map[string]string{}
		}
		for _, decl := range f.file.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							consts[dir][name.Name] = s
						}
					}
				}
			}
		}
	}
	msgs := map[string]string{}
	add := func(where, msg string) {
		if _, ok := msgs[msg]; !ok {
			msgs[msg] = where
		}
	}
	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		dir := filepath.Dir(f.rel)
		var stack []ast.Node
		ast.Inspect(f.file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := ast.Unparen(call.Fun).(type) {
			case *ast.SelectorExpr:
				if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "http" {
					return true // http.Error writes a response body
				}
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			default:
				return true
			}
			idx, ok := logSinks[name]
			if !ok || len(call.Args) <= idx {
				return true
			}
			arg := call.Args[idx]
			where := f.rel + ":" + strconv.Itoa(guardFset.Position(arg.Pos()).Line)
			switch a := arg.(type) {
			case *ast.BasicLit:
				if a.Kind == token.STRING {
					s, _ := strconv.Unquote(a.Value)
					add(where, s)
					return true
				}
			case *ast.Ident:
				if v, ok := consts[dir][a.Name]; ok {
					add(where, v)
					return true
				}
				if forwardsParam(stack, a.Name) {
					return true
				}
			}
			key := f.rel + " " + types.ExprString(arg)
			names, ok := fieldMessages[key]
			if !ok {
				t.Errorf("%s: log message %s is neither a literal, a constant nor a listed forwarding helper's parameter; "+
					"add the helper to logSinks (or the expression to fieldMessages) in internal/observability/event_names_test.go",
					where, types.ExprString(arg))
				return true
			}
			for _, c := range names {
				v, ok := consts[dir][c]
				if !ok {
					t.Errorf("%s: constant %s of fieldMessages[%q] not found", where, c, key)
					continue
				}
				add(where+" ("+c+")", v)
			}
			return true
		})
	}
	// A field's values come from the code, not the list: every value its
	// file assigns to the field must be a constant fieldMessages lists for
	// it, or a snake_case literal, itself a message.
	for key, names := range fieldMessages {
		rel, expr, _ := strings.Cut(key, " ")
		field := expr[strings.LastIndex(expr, ".")+1:]
		for _, f := range files {
			if f.rel != rel {
				continue
			}
			for _, v := range fieldValues(f.file, field) {
				where := f.rel + ":" + strconv.Itoa(guardFset.Position(v.Pos()).Line)
				if id, ok := v.(*ast.Ident); ok && slices.Contains(names, id.Name) {
					continue
				}
				if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, _ := strconv.Unquote(lit.Value); snakeCase.MatchString(s) {
						add(where, s)
						continue
					}
				}
				t.Errorf("%s: %s is assigned %s, which fieldMessages does not list; "+
					"make it a snake_case event constant and add it there (internal/observability/event_names_test.go)",
					where, expr, types.ExprString(v))
			}
		}
	}
	return msgs
}

// fieldValues returns the values file assigns to a field named field: the
// right side of each assignment to a selector of that name (h.step, h.err =
// stepReap, err), and the value of each composite-literal key of that name.
func fieldValues(file *ast.File, field string) []ast.Expr {
	var values []ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == field {
					if len(s.Rhs) == len(s.Lhs) {
						values = append(values, s.Rhs[i])
					} else {
						values = append(values, s.Rhs[0]) // h.step, h.err = f(): no listed constant
					}
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := s.Key.(*ast.Ident); ok && key.Name == field {
				values = append(values, s.Value)
			}
		}
		return true
	})
	return values
}

// Rulings R-H3 and R-H6 (phase H1): the 43 messages are snake_case. No Go
// file under internal/ or cmd/ — tests included — carries an old prose
// message; each new name is a log message of production code (as
// logMessages reads them, the reaper's step constants included); and
// docs/log-fields.md gives each one a row.
func TestSweptEventNames(t *testing.T) {
	for _, f := range parseTree(t) {
		for _, e := range sweptEvents {
			if strings.Contains(f.src, e.old) {
				t.Errorf("%s still says %q; rename it to %s", f.rel, e.old, e.event)
			}
		}
	}
	msgs := logMessages(t)
	documented := documentedEvents(t)
	for _, e := range sweptEvents {
		if _, ok := msgs[e.event]; !ok {
			t.Errorf("no production code emits %s (was %q)", e.event, e.old)
		}
		if !documented[e.event] {
			t.Errorf("docs/log-fields.md has no row for %s", e.event)
		}
	}
}

// documentedEvents returns the names docs/log-fields.md gives a row: each
// backquoted name in the first cell of a table row (one row may name
// several events, as the suite reaper's failures share one). A mention
// elsewhere — in another row's meaning, say — is no row.
func documentedEvents(t *testing.T) map[string]bool {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "log-fields.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(doc), "\n") {
		cells := strings.SplitN(line, "|", 3)
		if !strings.HasPrefix(line, "| ") || len(cells) < 3 {
			continue
		}
		parts := strings.Split(cells[1], "`")
		for i := 1; i < len(parts); i += 2 {
			out[parts[i]] = true
		}
	}
	return out
}

// Ruling R-H6 (phase H1): every log message of production code is a
// snake_case event or one of main's prose messages, so a new prose message
// fails the build (logMessages says which messages it can read). Each
// allowlisted prose message must still be logged, so the list never
// outlives a rename.
func TestLogMessagesAreSnakeCase(t *testing.T) {
	allowed := map[string]bool{}
	for _, m := range mainProseMessages {
		allowed[m] = true
	}
	msgs := logMessages(t)
	for msg, where := range msgs {
		if !snakeCase.MatchString(msg) && !allowed[msg] {
			t.Errorf("%s: log message %q is not snake_case (and not one of main's prose messages); "+
				"use a snake_case event name and document it in docs/log-fields.md", where, msg)
		}
	}
	for _, m := range mainProseMessages {
		if _, ok := msgs[m]; !ok {
			t.Errorf("main's prose message %q is no longer logged: remove it from mainProseMessages", m)
		}
	}
}

// forwardsParam reports whether name is a parameter of the innermost
// function enclosing the top of stack, and that function forwards it: a
// declared function named in logSinks, or a function literal that is the
// value of a key named in logSinks (internal/proxy's LogFn callbacks).
func forwardsParam(stack []ast.Node, name string) bool {
	for i := len(stack) - 1; i >= 0; i-- {
		var params *ast.FieldList
		forwarder := false
		switch fn := stack[i].(type) {
		case *ast.FuncDecl:
			params, forwarder = fn.Type.Params, hasSink(fn.Name.Name)
		case *ast.FuncLit:
			params = fn.Type.Params
			if i > 0 {
				if kv, ok := stack[i-1].(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						forwarder = hasSink(key.Name)
					}
				}
			}
		default:
			continue
		}
		if !forwarder {
			return false
		}
		for _, field := range params.List {
			for _, p := range field.Names {
				if p.Name == name {
					return true
				}
			}
		}
		return false
	}
	return false
}

func hasSink(name string) bool {
	_, ok := logSinks[name]
	return ok
}

// Rulings R-H3 and R-H6, the guard's blind spot (execution review G-10):
// logMessages reads the message of each call of a logSinks name, so a log
// method taken as a value (info := logger.Info; info("prose message")) is
// never read, and a prose message logged through it would pass
// TestLogMessagesAreSnakeCase. Production code calls its loggers' methods
// directly; this pins that, so the guard's reading stays complete. Without
// type information it judges by shape: a Debug/Info/Warn/Error/…Context/
// Log/LogAttrs selector on a receiver whose expression names a logger, used
// as a value — assigned, passed, returned, sent or stored, parenthesized or
// not — rather than called.
func TestLogMethodsAreOnlyCalledDirectly(t *testing.T) {
	methods := map[string]bool{"Debug": true, "Info": true, "Warn": true, "Error": true, "DebugContext": true,
		"InfoContext": true, "WarnContext": true, "ErrorContext": true, "Log": true, "LogAttrs": true}
	for _, f := range parseTree(t) {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		var stack []ast.Node
		ast.Inspect(f.file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !methods[sel.Sel.Name] || len(stack) < 2 ||
				!strings.Contains(strings.ToLower(types.ExprString(sel.X)), "log") {
				return true
			}
			// The parent that uses it, past any parentheses: (logger.Info)("…")
			// is a call, x := (logger.Info) a value.
			use, i := ast.Node(n), len(stack)-2
			for ; i > 0; i-- {
				if _, ok := stack[i].(*ast.ParenExpr); !ok {
					break
				}
				use = stack[i]
			}
			valueUse := false
			switch p := stack[i].(type) {
			case *ast.AssignStmt, *ast.ReturnStmt, *ast.CompositeLit, *ast.SendStmt:
				valueUse = true
			case *ast.KeyValueExpr:
				valueUse = p.Value == use
			case *ast.ValueSpec:
				for _, v := range p.Values {
					valueUse = valueUse || v == use
				}
			case *ast.CallExpr:
				valueUse = p.Fun != use
			}
			if valueUse {
				t.Errorf("%s:%s: %s taken as a value; call the log method directly so TestLogMessagesAreSnakeCase reads its message",
					f.rel, strconv.Itoa(guardFset.Position(sel.Pos()).Line), types.ExprString(sel))
			}
			return true
		})
	}
}
