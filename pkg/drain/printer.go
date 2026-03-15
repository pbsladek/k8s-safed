package drain

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// LogFormat selects the output style for drain progress messages.
type LogFormat string

const (
	// LogFormatPlain emits human-readable lines with a timestamp, a fixed-width
	// subject column, a level label, and a message. This is the default.
	//
	// Every line follows the same pattern so the output is easy to grep:
	//
	//   [safed] 15:04:05  Deployment/default/api       start   Rolling restart [1/3]
	//   [safed] 15:04:10  Deployment/default/api       poll    rollout updated=2/3 ready=1/3 available=1/3
	//   [safed] 15:04:15  Deployment/default/api       done    Complete (10s)
	//
	// Useful grep patterns:
	//   grep "start"                  – all workloads that began restarting
	//   grep "done"                   – all completions
	//   grep "poll"                   – progress detail only
	//   grep "Deployment/default/api" – every line for one workload
	//   grep "dryrun"                 – dry-run preview lines
	LogFormatPlain LogFormat = "plain"

	// LogFormatJSON emits one JSON object per line, suitable for ingestion by
	// log aggregators (Loki, Splunk, Datadog, ELK, etc.).
	//
	// Schema:
	//   ts      – RFC3339 UTC timestamp
	//   level   – "info" | "start" | "done" | "poll" | "dryrun" | "warn"
	//   subject – node name for drain-level events, "Kind/namespace/name" for workloads
	//   msg     – human-readable message
	//
	// Example:
	//   {"ts":"2026-03-15T15:04:05Z","level":"start","subject":"Deployment/default/api","msg":"Rolling restart [1/3]"}
	//   {"ts":"2026-03-15T15:04:10Z","level":"poll","subject":"Deployment/default/api","msg":"rollout updated=2/3 ready=1/3 available=1/3"}
	//   {"ts":"2026-03-15T15:04:15Z","level":"done","subject":"Deployment/default/api","msg":"Complete (10s)"}
	LogFormatJSON LogFormat = "json"
)

// subjectWidth is the minimum column width for the subject field in plain
// format. Subjects shorter than this are right-padded; longer ones overflow
// without truncation.
const subjectWidth = 32

// Printer writes structured drain progress messages.
// All methods are safe for concurrent use from multiple goroutines.
type Printer struct {
	out    io.Writer
	mu     sync.Mutex
	format LogFormat
}

// NewPrinter returns a Printer that writes to stdout in plain format.
func NewPrinter() *Printer { return &Printer{out: os.Stdout, format: LogFormatPlain} }

// NewPrinterTo returns a Printer that writes to w in plain format.
// Primarily used in tests to capture or discard output.
func NewPrinterTo(w io.Writer) *Printer { return &Printer{out: w, format: LogFormatPlain} }

// NewPrinterWithFormat returns a Printer that writes to w using the given LogFormat.
func NewPrinterWithFormat(w io.Writer, f LogFormat) *Printer {
	return &Printer{out: w, format: f}
}

// ---------------------------------------------------------------------------
// Public methods
// All are goroutine-safe. "f" variants accept a printf-style format string.
// ---------------------------------------------------------------------------

// Info logs a general informational message (drain-level or workload-level).
func (p *Printer) Info(subject, msg string) { p.emit("info", subject, msg) }

// Infof logs a formatted informational message.
func (p *Printer) Infof(subject, format string, args ...any) {
	p.emit("info", subject, fmt.Sprintf(format, args...))
}

// Start logs the beginning of a workload operation (e.g. "Rolling restart").
func (p *Printer) Start(subject, msg string) { p.emit("start", subject, msg) }

// Startf logs a formatted start message.
func (p *Printer) Startf(subject, format string, args ...any) {
	p.emit("start", subject, fmt.Sprintf(format, args...))
}

// Done logs the successful completion of an operation.
func (p *Printer) Done(subject, msg string) { p.emit("done", subject, msg) }

// Donef logs a formatted completion message.
func (p *Printer) Donef(subject, format string, args ...any) {
	p.emit("done", subject, fmt.Sprintf(format, args...))
}

// Poll logs the result of a periodic status check (rollout status, pod count).
// Poll messages are the most frequent; filter them in or out with grep "poll".
func (p *Printer) Poll(subject, msg string) { p.emit("poll", subject, msg) }

// Pollf logs a formatted poll message.
func (p *Printer) Pollf(subject, format string, args ...any) {
	p.emit("poll", subject, fmt.Sprintf(format, args...))
}

// DryRun logs an action that would be taken (dry-run mode only).
func (p *Printer) DryRun(subject, msg string) { p.emit("dryrun", subject, msg) }

// DryRunf logs a formatted dry-run action.
func (p *Printer) DryRunf(subject, format string, args ...any) {
	p.emit("dryrun", subject, fmt.Sprintf(format, args...))
}

// Warn logs a preflight warning — a condition that could cause downtime or
// requires operator attention before the drain proceeds.
func (p *Printer) Warn(subject, msg string) { p.emit("warn", subject, msg) }

// Warnf logs a formatted preflight warning.
func (p *Printer) Warnf(subject, format string, args ...any) {
	p.emit("warn", subject, fmt.Sprintf(format, args...))
}

// Elapsed logs a completion message with the duration since since appended.
func (p *Printer) Elapsed(since time.Time, subject, msg string) {
	elapsed := time.Since(since).Round(time.Millisecond)
	p.emit("done", subject, fmt.Sprintf("%s (%s)", msg, elapsed))
}

// ---------------------------------------------------------------------------
// Internal rendering
// ---------------------------------------------------------------------------

func (p *Printer) emit(level, subject, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	switch p.format {
	case LogFormatJSON:
		rec := map[string]any{
			"ts":      now.UTC().Format(time.RFC3339),
			"level":   level,
			"subject": subject,
			"msg":     msg,
		}
		data, _ := json.Marshal(rec)
		fmt.Fprintln(p.out, string(data))

	default: // LogFormatPlain
		ts := now.Format("15:04:05")
		// Pad subject to subjectWidth so that message columns align across
		// node-level ("node", 4 chars) and workload-level lines.
		fmt.Fprintf(p.out, "[safed] %s  %-*s  %-6s  %s\n",
			ts, subjectWidth, subject, level, msg)
	}
}
