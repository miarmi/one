package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

// A panic in background work must not take the app down. Every goroutine the
// app starts, and every loop that works through records, runs one unit of
// work (one run, one message, one event) under runUnit or tryUnit: a panic is
// logged at error with the unit's name and a stack of the app's own frames
// (never a value that could hold message words), and the loop goes on with
// the next unit after its normal interval. HTTP handlers are covered by
// appkit.LogRequests (500 and an error line), which net/http never sees.

// panicSeen lets a unit test see a recovered panic; nil in the app.
var panicSeen atomic.Pointer[func(unit string, v any)]

// unitPanic is what a panic inside one record's work becomes: an error that
// belongs to that record, so the run goes on with the next one.
type unitPanic struct{ unit string }

func (e *unitPanic) Error() string { return "panic in " + e.unit }

// panicText says what panicked without quoting data: the text of a runtime
// error (an index, a nil pointer: numbers and words of the code), otherwise
// only the type of the value.
func panicText(v any) string {
	if re, ok := v.(runtime.Error); ok {
		return re.Error()
	}
	return fmt.Sprintf("%T", v)
}

// ownFrames is the stack at the point of a panic, trimmed to the app's own
// functions: "function file:line", at most ten.
func ownFrames() []string {
	pcs := make([]uintptr, 48)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var out []string
	for {
		f, more := frames.Next()
		if strings.HasPrefix(f.Function, "lab/apps/one") && !strings.Contains(f.Function, ".recoverUnit") &&
			!strings.Contains(f.Function, ".runUnit") && !strings.Contains(f.Function, ".tryUnit") {
			out = append(out, fmt.Sprintf("%s %s:%d", strings.TrimPrefix(f.Function, "lab/apps/one."), filepath.Base(f.File), f.Line))
			if len(out) == 10 {
				break
			}
		}
		if !more {
			break
		}
	}
	return out
}

// recoverUnit is deferred by runUnit and tryUnit.
func (a *app) recoverUnit(unit string, panicked *bool) {
	v := recover()
	if v == nil {
		return
	}
	*panicked = true
	if h := panicSeen.Load(); h != nil {
		(*h)(unit, v)
	}
	a.logger().Error("panic in background work", "unit", unit, "panic", panicText(v), "stack", ownFrames())
}

// runUnit runs one unit of background work; a panic is logged and the caller
// goes on. It says whether the unit panicked.
func (a *app) runUnit(unit string, fn func()) (panicked bool) {
	defer a.recoverUnit(unit, &panicked)
	fn()
	return false
}

// tryUnit runs one record's work; a panic becomes a *unitPanic error, which
// the callers treat as that record's own failure.
func (a *app) tryUnit(unit string, fn func() error) (err error) {
	panicked := false
	defer func() {
		if panicked {
			err = &unitPanic{unit}
		}
	}()
	defer a.recoverUnit(unit, &panicked)
	return fn()
}
