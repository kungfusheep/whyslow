package main

// tui.go is the interactive front end: pick something detected in the project, watch it record,
// then read the report and open its profile, without leaving the terminal. it shares the look of
// the mail and calendar apps: one palette with live theme switching, filled panes instead of
// borders, a bright-to-muted type ramp, lowercase muted key hints and a vignetted help overlay.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	. "github.com/kungfusheep/glyph"
)

// th is the live palette: the views hold pointers into it, so applying a theme restyles everything
// on the next frame. the log colours its lines on its own goroutine, so it reads logTheme instead.
var (
	th       Theme
	logTheme atomic.Pointer[Theme]
	rowStyle Style
	selStyle Style
)

type toastKind int

const (
	toastInfo toastKind = iota
	toastSuccess
	toastWarning
	toastError
)

type toast struct {
	Text    string
	Kind    toastKind
	Opacity float64
	at      time.Time
}

const (
	toastTTL  = 2800 * time.Millisecond
	toastFade = 700 * time.Millisecond
)

type session struct {
	app  *App
	o    options
	logW io.Writer
	log  *LogC

	running bool
	status  string
	cancel  context.CancelFunc
	done    chan struct{}
	last    *candidate
	report  *report
	pprof   []*exec.Cmd

	themeName string
	helpOpen  bool
	helpRef   NodeRef
	toasts    []toast
	ticking   bool
}

func runTUI(o options, root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	cands := detect(root)
	pr, pw := io.Pipe()
	s := &session{app: NewApp(), o: o, logW: pw, status: "nothing recorded yet"}
	s.themeName = loadSettings().Theme
	t, ok := themeByName(s.themeName)
	if !ok {
		s.themeName, t = "dark", themeDark
	}
	s.applyTheme(t)

	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(abs, home) {
		abs = "~" + abs[len(home):]
	}
	where := fmt.Sprintf("%s  %d to profile", abs, len(cands))
	if len(cands) == 0 {
		where = abs + "  nothing found: type a command, ctrl-r runs it"
	}
	nameWidth := 0
	for _, c := range cands {
		nameWidth = max(nameWidth, len(c.Name))
	}
	var list *FilterListC[candidate]
	s.app.View("pick", VBox.Fill(&th.BG).PaddingTRBL(1, 2, 1, 2)(
		header("enter profile  ctrl-g help", Text(where).FG(&th.Subtle)),
		SpaceH(1),
		VBox.Grow(1).Fill(&th.ThreadBG).PaddingTRBL(1, 2, 1, 2)(
			FilterList(&cands, func(c *candidate) string { return c.Where + " " + c.Name }).
				Render(func(c *candidate) Component {
					return HBox.Gap(2)(
						Text("▐").FG(kindColor(&c.Kind)),
						Text(&c.Kind).FG(&th.Dim).Width(4),
						Text(&c.Name).Width(nameWidth),
						Text(&c.Where).FG(&th.Subtle),
					)
				}).
				Placeholder("type to filter").
				Marker("  ").
				Style(&rowStyle).
				SelectedStyle(&selStyle).
				Ref(func(f *FilterListC[candidate]) { list = f }),
		),
		s.helpOverlay(),
		s.toastFeed(),
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
				switch {
				case s.helpOpen:
					s.helpOpen = false
				case list.Active():
					list.Clear()
				default:
					s.app.Stop()
				}
			}),
			// the filter input takes printable keys here, so help and themes sit on control keys
			Key("<C-t>", s.nextTheme),
			Key("<C-g>", s.toggleHelp),
			Key("<C-c>", s.app.Stop),
		),
	))

	s.app.View("run", VBox.Fill(&th.BG).PaddingTRBL(1, 2, 1, 2)(
		header("p pprof  esc back  ? help",
			If(&s.running).Then(Spinner().Frames(SpinnerDots).FG(&th.Subtle)),
			If(&s.running).Then(SpaceW(1)),
			Text(&s.status).FG(&th.Subtle),
		),
		SpaceH(1),
		VBox.Grow(1).Fill(&th.ThreadBG).PaddingTRBL(1, 2, 1, 2)(
			Log(pr).Grow(1).BindVimNav().Colorize(reportSpans).Ref(func(l *LogC) { s.log = l }),
		),
		s.helpOverlay(),
		s.toastFeed(),
		On(
			Key("<Esc>", func() {
				if s.helpOpen {
					s.helpOpen = false
				} else {
					s.app.Go("pick")
				}
			}),
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
			Key("T", s.nextTheme),
			Key("?", s.toggleHelp),
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

// header is the line every screen opens with: the app name, its context, and muted key hints.
func header(hints string, info ...Component) Component {
	row := []Component{Text("whyslow").FG(&th.Bright).Bold(), SpaceW(2)}
	row = append(row, info...)
	return HBox(append(row, Space(), Text(hints).FG(&th.Muted))...)
}

func kindColor(kind *string) any {
	return Match(kind,
		Eq("go", &th.Info),
		Eq("zig", &th.Warning),
		Eq("file", &th.Success),
	).Default(&th.Accent)
}

// helpOverlay and toastFeed are shared by both views. they sit directly in each view's root, as in
// the mail app: overlays wrapped in another box lose their screen placement.
func (s *session) helpOverlay() Component {
	return If(&s.helpOpen).Then(
		Overlay.Centered()(
			VBox.Width(64).Fill(&th.BG).PaddingVH(1, 2).NodeRef(&s.helpRef).
				Opacity(In(Animate(1.0)).Out(Animate(0))).
				Gap(1)(
				Text("keyboard").FG(&th.Bright).Bold(),
				HBox(
					helpSection("pick", 7, &pickKeys),
					helpSection("report", 9, &reportKeys),
				),
				ScreenEffect(
					SEVignette().Strength(In(Animate.From(0)(0.55)).Out(Animate(0))).Dodge(&s.helpRef).Smooth(),
					SEDropShadow().Focus(&s.helpRef),
				),
			),
		),
	)
}

func (s *session) toastFeed() Component {
	return Overlay.BottomRight().Offset(-2, -1)(
		VBox.Width(56).Gap(1)(
			ForEach(&s.toasts, func(t *toast) Component {
				return HBox.Width(56).Opacity(&t.Opacity)(
					Space(),
					Text("● ").FG(Match(&t.Kind,
						Eq(toastSuccess, &th.Success),
						Eq(toastWarning, &th.Warning),
						Eq(toastError, &th.Error),
					).Default(&th.Info)),
					Text(&t.Text).FG(&th.Bright),
				)
			}),
		),
	)
}

type helpKey struct{ key, desc string }

var pickKeys = []helpKey{
	{"type", "filter"},
	{"↑ ↓", "move"},
	{"enter", "profile"},
	{"ctrl-r", "run typed text"},
	{"esc", "clear / quit"},
	{"ctrl-t", "next theme"},
	{"ctrl-g", "this help"},
}

var reportKeys = []helpKey{
	{"j / k", "scroll"},
	{"ctrl-d/u", "page"},
	{"g / G", "top / bottom"},
	{"p", "pprof in browser"},
	{"i", "open in instruments"},
	{"r", "re-run"},
	{"x", "stop recording"},
	{"T", "next theme"},
	{"esc", "back to the list"},
	{"q", "quit"},
}

func helpSection(title string, keyWidth int, rows *[]helpKey) Component {
	return VBox.Grow(1)(
		Text(title).FG(&th.Subtle),
		ForEach(rows, func(r *helpKey) Component {
			return HBox.Gap(2)(
				Text(&r.key).FG(&th.FG).Width(keyWidth),
				Text(&r.desc).FG(&th.Subtle),
			)
		}),
	)
}

func (s *session) toggleHelp() { s.helpOpen = !s.helpOpen }

func (s *session) applyTheme(t Theme) {
	th = t
	logTheme.Store(&t)
	rowStyle = Style{FG: t.FG}
	selStyle = Style{FG: t.Bright, BG: t.SelBG, Attr: AttrBold}
	s.app.SetDefaultStyle(Style{FG: t.FG, BG: t.BG})
	if s.log != nil {
		s.log.Refresh()
	}
}

func (s *session) nextTheme() {
	s.themeName = nextThemeName(s.themeName)
	t, _ := themeByName(s.themeName)
	s.applyTheme(t)
	if err := saveSettings(settings{Theme: s.themeName}); err != nil {
		s.notify(toastError, "couldn't save the theme: "+err.Error())
		return
	}
	s.notify(toastInfo, "theme: "+themeLabelByName(s.themeName))
}

// notify shows a toast that fades out. the fade ticks only while toasts are on screen, so an idle
// tui doesn't render (and steal cycles) during a recording.
func (s *session) notify(kind toastKind, text string) {
	s.toasts = append(s.toasts, toast{Text: text, Kind: kind, Opacity: 1, at: time.Now()})
	if len(s.toasts) > 4 {
		s.toasts = s.toasts[len(s.toasts)-4:]
	}
	if s.ticking {
		return
	}
	s.ticking = true
	go func() {
		for done := false; !done; {
			time.Sleep(50 * time.Millisecond)
			ch := make(chan bool)
			s.app.Apply(func() { ch <- s.fadeToasts(time.Now()) })
			done = <-ch
		}
	}()
}

// fadeToasts updates each toast's opacity and drops expired ones, reporting when none are left.
func (s *session) fadeToasts(now time.Time) bool {
	keep := s.toasts[:0]
	for _, t := range s.toasts {
		age := now.Sub(t.at)
		if age >= toastTTL {
			continue
		}
		t.Opacity = min(1, float64(toastTTL-age)/float64(toastFade))
		keep = append(keep, t)
	}
	s.toasts = keep
	s.ticking = len(keep) > 0
	return !s.ticking
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
	s.status = fmt.Sprintf("profiling %s  %s", c.Name, c.Where)
	fmt.Fprintf(s.logW, "%s\n", bold(fmt.Sprintf("%s  %s  %s", c.Name, c.Where, time.Now().Format("15:04:05"))))

	go func() {
		defer close(s.done)
		r, err := run(ctx, o, args, s.logW)
		if err == nil {
			r.write(s.logW)
		} else {
			fmt.Fprintf(s.logW, "whyslow: %s\n", err)
		}
		fmt.Fprintln(s.logW)
		s.app.Apply(func() {
			s.running, s.cancel, s.report = false, nil, r
			switch {
			case ctx.Err() != nil:
				s.status = c.Name + " stopped"
				s.notify(toastWarning, "stopped "+c.Name)
			case err != nil:
				s.status = c.Name + " failed"
				s.notify(toastError, c.Name+" failed")
			default:
				s.status = c.Name + "  done"
				s.notify(toastSuccess, "profiled "+c.Name)
			}
		})
	}()
}

func (s *session) openPprof() {
	if s.report == nil {
		s.notify(toastWarning, "nothing to open yet")
		return
	}
	cmd := exec.Command("go", "tool", "pprof", "-http=localhost:", s.report.pprof)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		s.notify(toastError, "pprof: "+err.Error())
		return
	}
	s.pprof = append(s.pprof, cmd)
	s.notify(toastInfo, "pprof opening in your browser")
}

func (s *session) openTrace() {
	if s.report == nil {
		s.notify(toastWarning, "nothing to open yet")
		return
	}
	if err := exec.Command("open", s.report.trace).Run(); err != nil {
		s.notify(toastError, "open: "+err.Error())
		return
	}
	s.notify(toastInfo, "trace opening in instruments")
}

// reportSpans colours a report line with the palette's roles: the report's bold becomes bright,
// its dim becomes the dim role, warnings and errors take their colours, bars take info and the
// "concentrates" flags take the accent.
func reportSpans(line string) []Span {
	line = expandTabs(line)
	t := logTheme.Load()
	lineFG := t.FG
	switch trimmed := strings.TrimSpace(line); {
	case strings.Contains(line, "⚠"):
		lineFG = t.Warning
	case strings.HasPrefix(trimmed, "whyslow: ") || strings.HasSuffix(trimmed, " skipped"):
		lineFG = t.Error
	}
	var out []Span
	for _, sp := range ansiSpans(line) {
		st := Style{FG: lineFG, Attr: sp.Style.Attr &^ AttrDim}
		switch {
		case sp.Style.Attr&AttrBold != 0 && lineFG == t.FG:
			st.FG = t.Bright
		case sp.Style.Attr&AttrDim != 0 && strings.Contains(sp.Text, "←"):
			st.FG = t.Accent
		case sp.Style.Attr&AttrDim != 0:
			st.FG = t.Dim
		}
		out = append(out, splitBars(sp.Text, st, t.Info)...)
	}
	return out
}

// expandTabs replaces tabs with spaces to the next 8-column stop (go's benchmark lines and assembly
// source carry tabs). a raw tab moves the terminal cursor without drawing, which leaves stale cells
// behind. escape sequences take no columns.
func expandTabs(line string) string {
	if !strings.Contains(line, "\t") {
		return line
	}
	var b strings.Builder
	col, inEsc := 0, false
	for _, r := range line {
		switch {
		case inEsc:
			inEsc = r != 'm'
		case r == '\x1b':
			inEsc = true
		case r == '\t':
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		default:
			col++
		}
		b.WriteRune(r)
	}
	return b.String()
}

// splitBars gives runs of █ their own span in the bar colour.
func splitBars(text string, st Style, bar Color) []Span {
	var out []Span
	for text != "" {
		i := strings.Index(text, "█")
		if i < 0 {
			return append(out, Span{Text: text, Style: st})
		}
		if i > 0 {
			out = append(out, Span{Text: text[:i], Style: st})
		}
		j := i
		for strings.HasPrefix(text[j:], "█") {
			j += len("█")
		}
		bs := st
		bs.FG = bar
		out = append(out, Span{Text: text[i:j], Style: bs})
		text = text[j:]
	}
	return out
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
