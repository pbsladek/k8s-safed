package drain

import (
	"fmt"
	"io"
	"os"
	"time"
)

// Printer writes structured drain progress messages to an output stream.
type Printer struct {
	out io.Writer
}

// NewPrinter returns a Printer that writes to stdout.
func NewPrinter() *Printer {
	return &Printer{out: os.Stdout}
}

// NewPrinterTo returns a Printer that writes to w.
func NewPrinterTo(w io.Writer) *Printer {
	return &Printer{out: w}
}

func (p *Printer) Infof(format string, args ...any) {
	fmt.Fprintf(p.out, "[safed] "+format+"\n", args...)
}

func (p *Printer) DryRun(format string, args ...any) {
	fmt.Fprintf(p.out, "[safed] (dry-run) "+format+"\n", args...)
}

func (p *Printer) Step(step, total int, format string, args ...any) {
	prefix := fmt.Sprintf("[safed] [%d/%d] ", step, total)
	fmt.Fprintf(p.out, prefix+format+"\n", args...)
}

func (p *Printer) Elapsed(start time.Time, format string, args ...any) {
	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Fprintf(p.out, "[safed] "+format+" (%s)\n", append(args, elapsed)...)
}
