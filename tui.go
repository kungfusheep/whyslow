package main

// tui.go is the interactive front end: pick something detected in the project, watch it record,
// then read the report and open its profile, without leaving the terminal.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	. "github.com/kungfusheep/glyph"
)

type session struct {
	app  *App
	o    options
	logW io.Writer

	running bool
	status  string
	cancel  context.CancelFunc
	done    chan struct{}
	last    *candidate
	report  *report
	pprof   []*exec.Cmd
}

func runTUI(o options, root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	cands := detect(root)
	pr, pw := io.Pipe()
	s := &session{app: NewApp(), o: o, logW: pw, status: "nothing recorded yet"}

	header := fmt.Sprintf("whyslow · %s · %d to profile", abs, len(cands))
	pickHelp := "enter profile · ctrl-r profile the typed command · esc clear/quit"
	if len(cands) == 0 {
		pickHelp = "nothing detected: type a command and press ctrl-r, or list targets in .whyslow"
	}
	nameWidth := 0
	for _, c := range cands {
		nameWidth = max(nameWidth, len(c.Name))
	}
	var list *FilterListC[candidate]
	s.app.View("pick", VBox(
		Text(header).Bold(),
		FilterList(&cands, func(c *candidate) string { return c.Where + " " + c.Name }).
			Render(func(c *candidate) Component {
				return HBox.Gap(2)(
					Text(&c.Kind).FG(BrightBlack).Width(4),
					Text(&c.Name).Bold().Width(nameWidth),
					Text(&c.Where).FG(BrightBlack),
				)
			}).
			Placeholder("filter").
			Border(BorderRounded).
			Title("profile").
			Ref(func(f *FilterListC[candidate]) { list = f }),
		Text(pickHelp).FG(BrightBlack),
		On(
			Key("<Enter>", func() {
				if c := list.Selected(); c != nil {
					s.start(c)
				}
			}),
			Key("<C-r>", func() {
				if argv := strings.Fields(list.Query()); len(argv) > 0 {
					s.start(&candidate{Kind: "cmd", Name: filepath.Base(argv[0]), Where: list.Query(), argv: argv})
				}
			}),
			Key("<Esc>", func() {
				if list.Active() {
					list.Clear()
				} else {
					s.app.Stop()
				}
			}),
			Key("<C-c>", s.app.Stop),
		),
	))

	s.app.View("run", VBox(
		HBox.Gap(1)(
			If(&s.running).Then(Spinner().FG(Cyan)),
			Text(&s.status).Bold(),
		),
		Log(pr).Grow(1).BindVimNav().Colorize(ansiSpans),
		Text("j/k ctrl-d/u g/G scroll · p pprof in browser · i trace in Instruments · r re-run · x stop · esc back · q quit").FG(BrightBlack),
		On(
			Key("<Esc>", func() { s.app.Go("pick") }),
			Key("r", func() {
				if s.last != nil {
					s.start(s.last)
				}
			}),
			Key("x", func() {
				if s.cancel != nil {
					s.cancel()
				}
			}),
			Key("p", s.openPprof),
			Key("i", s.openTrace),
			Key("q", s.app.Stop),
			Key("<C-c>", s.app.Stop),
		),
	))

	err = s.app.RunFrom("pick")

	if s.running {
		fmt.Fprintln(os.Stderr, "stopping the recording...")
		s.cancel()
		<-s.done
	}
	for _, c := range s.pprof {
		// go tool pprof runs the real pprof as a child, so kill the whole group
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		c.Wait()
	}
	pw.Close()
	return err
}

// start runs a candidate in the background, streaming progress and then the report into the log.
func (s *session) start(c *candidate) {
	s.app.Go("run")
	if s.running {
		return
	}
	o := s.o
	args := c.argv
	if c.Kind == "go" {
		o.bench = "^" + c.bench + "$"
		args = []string{c.Where}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.running, s.cancel, s.done, s.last, s.report = true, cancel, make(chan struct{}), c, nil
	s.status = fmt.Sprintf("profiling %s · %s", c.Name, c.Where)
	fmt.Fprintf(s.logW, "%s\n", bold(fmt.Sprintf("── %s · %s · %s ──", c.Name, c.Where, time.Now().Format("15:04:05"))))

	go func() {
		defer close(s.done)
		r, err := run(ctx, o, args, s.logW)
		if err == nil {
			r.write(s.logW)
		} else {
			fmt.Fprintf(s.logW, "%s\n", bold("whyslow: "+err.Error()))
		}
		fmt.Fprintln(s.logW)
		s.app.Apply(func() {
			s.running, s.cancel, s.report = false, nil, r
			switch {
			case ctx.Err() != nil:
				s.status = c.Name + " stopped"
			case err != nil:
				s.status = c.Name + " failed"
			default:
				s.status = c.Name + " done · " + filepath.Dir(r.pprof)
			}
		})
	}()
}

func (s *session) openPprof() {
	if s.report == nil {
		return
	}
	cmd := exec.Command("go", "tool", "pprof", "-http=localhost:", s.report.pprof)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		s.status = "pprof: " + err.Error()
		return
	}
	s.pprof = append(s.pprof, cmd)
	s.status = "pprof opening in your browser (it closes when whyslow exits)"
}

func (s *session) openTrace() {
	if s.report == nil {
		return
	}
	if err := exec.Command("open", s.report.trace).Run(); err != nil {
		s.status = "open: " + err.Error()
	}
}

// ansiSpans turns the report's own bold and dim escapes into styled spans, so the tui shows the
// same emphasis as the terminal report.
func ansiSpans(line string) []Span {
	var spans []Span
	var st Style
	for line != "" {
		i := strings.Index(line, "\x1b[")
		if i < 0 {
			spans = append(spans, Span{Text: line, Style: st})
			break
		}
		if i > 0 {
			spans = append(spans, Span{Text: line[:i], Style: st})
		}
		line = line[i+2:]
		j := strings.IndexByte(line, 'm')
		if j < 0 {
			break
		}
		switch line[:j] {
		case "1":
			st.Attr |= AttrBold
		case "2":
			st.Attr |= AttrDim
		case "0", "":
			st = Style{}
		}
		line = line[j+1:]
	}
	return spans
}
