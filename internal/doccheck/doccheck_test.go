package doccheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const docsRoot = "../../docs"

var (
	// [name](target) where target is relative (not http, not a bare #anchor).
	linkRe = regexp.MustCompile(`\]\(([^)#\s]+)(#[^)\s]*)?\)`)
	// [file.go:N...](path/file.go#LN): a link to a line of code.
	codeRe = regexp.MustCompile(`\[([\w./]+\.go):(\d+)[^\]]*\]\(([^)#]+\.go)#L(\d+)\)`)
	declRe = regexp.MustCompile(`^(func|type|const|var)\b`)
	// Code that markdown doesn't render as links: fenced blocks and inline spans.
	fenceRe = regexp.MustCompile("(?s)```.*?```")
	spanRe  = regexp.MustCompile("`[^`\n]*`")
)

func markdownFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(docsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 10 {
		t.Fatalf("found only %d docs under %s; is the path right?", len(files), docsRoot)
	}
	return files
}

// Every relative link points at a file that exists.
func TestRelativeLinksResolve(t *testing.T) {
	for _, doc := range markdownFiles(t) {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		prose := spanRe.ReplaceAllString(fenceRe.ReplaceAllString(string(data), ""), "")
		for _, m := range linkRe.FindAllStringSubmatch(prose, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(doc), target)); err != nil {
				t.Errorf("%s: link to %s: %v", doc, target, err)
			}
		}
	}
}

// Every code-line link lands on a declaration, and its text agrees with its
// anchor. When code moves, this fails until the doc is updated.
func TestCodeLinksPointAtDeclarations(t *testing.T) {
	checked := 0
	for _, doc := range markdownFiles(t) {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, loc := range codeRe.FindAllStringSubmatchIndex(text, -1) {
			m := make([]string, len(loc)/2)
			for i := range m {
				m[i] = text[loc[2*i]:loc[2*i+1]]
			}
			textLine, _ := strconv.Atoi(m[2])
			anchor, _ := strconv.Atoi(m[4])
			path := filepath.Join(filepath.Dir(doc), m[3])
			if !strings.HasSuffix(m[3], m[1]) {
				t.Errorf("%s: %s: link text names %s but points at %s", doc, m[0], m[1], m[3])
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s: %s: %v", doc, m[0], err)
				continue
			}
			lines := strings.Split(string(src), "\n")
			if anchor < 1 || anchor > len(lines) {
				t.Errorf("%s: %s: %s has only %d lines", doc, m[0], m[3], len(lines))
				continue
			}
			if !declRe.MatchString(lines[anchor-1]) {
				t.Errorf("%s: %s: line %d is %q, not a declaration (code moved; update the link)",
					doc, m[0], anchor, strings.TrimSpace(lines[anchor-1]))
			} else if names := namedBefore(text, loc[0]); len(names) > 0 && !declaresAny(lines, anchor-1, names) {
				// The link lands on a declaration, but not of what the text
				// names (e.g. a link re-pointed to the wrong function).
				t.Errorf("%s: %s: line %d declares %q, but the text names %v",
					doc, m[0], anchor, strings.TrimSpace(lines[anchor-1]), names)
			}
			// The anchor must be one of the line numbers in the link text.
			nums := regexp.MustCompile(`\d+`).FindAllString(strings.SplitN(m[0], "](", 2)[0], -1)
			found := false
			for _, n := range nums {
				if v, _ := strconv.Atoi(n); v == anchor {
					found = true
				}
			}
			if !found && textLine != anchor {
				t.Errorf("%s: %s: text says line %d, anchor says %d", doc, m[0], textLine, anchor)
			}
			checked++
		}
	}
	if checked < 50 {
		t.Fatalf("checked only %d code links; the pattern may be broken", checked)
	}
	t.Logf("%d code links checked", checked)
}

// name is what a doc's backtick span names: "`Order.Validate`" is the
// method Validate with receiver Order, "`rest(o, qty)`" the function rest,
// "`book.Book.Submit`" the method Submit of Book.
type name struct{ recv, ident string }

var leadRe = regexp.MustCompile(`^\*?([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)`)

// namedBefore returns what the backtick spans just before the link at
// offset i name: those on the same line after any earlier link, so a link
// is judged by its own text ("**`Sequencer.Stamp`** ([...](...))" gives
// Stamp of Sequencer; "**`Role`, `CancelReason`** (...)" gives both, for a
// link that covers several declarations). Empty if there are none.
func namedBefore(doc string, i int) []name {
	lineStart := strings.LastIndex(doc[:i], "\n") + 1
	prefix := doc[lineStart:i]
	if j := strings.LastIndex(prefix, "]("); j >= 0 { // an earlier link on the line
		if k := strings.Index(prefix[j:], ")"); k >= 0 {
			prefix = prefix[j+k+1:]
		}
	}
	var names []name
	for _, span := range spanRe.FindAllString(prefix, -1) {
		m := leadRe.FindStringSubmatch(strings.Trim(span, "`"))
		if m == nil {
			continue
		}
		parts := strings.Split(m[1], ".")
		n := name{ident: parts[len(parts)-1]}
		if len(parts) > 1 {
			n.recv = parts[len(parts)-2]
		}
		names = append(names, n)
	}
	return names
}

var (
	methodRe = regexp.MustCompile(`^func \((?:[A-Za-z_]\w* )?\*?([A-Za-z_]\w*)(?:\[[^\]]*\])?\) ([A-Za-z_]\w*)`)
	funcRe   = regexp.MustCompile(`^func ([A-Za-z_]\w*)`)
	singleRe = regexp.MustCompile(`^(?:type|const|var) ([A-Za-z_]\w*)`)
	blockRe  = regexp.MustCompile(`^(?:type|const|var) \($`)
	memberRe = regexp.MustCompile(`^\s+([A-Za-z_]\w*)`)
)

// declaresAny reports whether lines[i], a declaration, declares one of
// names: a method of the same name (and receiver, when the text gives one),
// a function of that name (a qualifier like `fsutil.SyncDir` is then the
// package), a type the text names or names a member of, or, for a
// const/var/type block, a member of that name.
func declaresAny(lines []string, i int, names []name) bool {
	line := lines[i]
	for _, n := range names {
		switch {
		case methodRe.MatchString(line):
			m := methodRe.FindStringSubmatch(line)
			if m[2] == n.ident && (n.recv == "" || n.recv == m[1]) {
				return true
			}
		case funcRe.MatchString(line):
			if funcRe.FindStringSubmatch(line)[1] == n.ident {
				return true
			}
		case blockRe.MatchString(line):
			for _, l := range lines[i+1:] {
				if strings.HasPrefix(l, ")") {
					break
				}
				if m := memberRe.FindStringSubmatch(l); m != nil && (m[1] == n.ident || m[1] == n.recv) {
					return true
				}
			}
		case singleRe.MatchString(line):
			if d := singleRe.FindStringSubmatch(line)[1]; d == n.ident || d == n.recv {
				return true
			}
		}
	}
	return false
}

// The link check catches a link that lands on the wrong declaration. These
// three, found in the presentation pass, used to pass: the old check took
// every backtick identifier earlier on the line (including one before an
// earlier link, and parameters like `o`) and accepted a declaration line
// mentioning any of them.
func TestLinkTextMustNameTheDeclaration(t *testing.T) {
	cases := []struct {
		doc, decl string
		ok        bool
	}{
		// Wrong: `Order.Validate` linked to Instrument's Validate.
		{"- **`Order.Validate`** ([types.go:255](types.go#L255)). Field checks", "func (in Instrument) Validate() error {", false},
		// Wrong: `rest(o, qty)` linked to fillCheck, whose parameter is o.
		{"**`rest(o, qty)`** ([book.go:272](book.go#L272)):", "func (b *Book) fillCheck(o *Order) (ok bool, visited int) {", false},
		// Wrong: `setup` linked to Init, named by the earlier link's text.
		{"- **`Init`** ([clock.go:92](clock.go#L92)) and **`setup`** ([clock.go:92](clock.go#L92)): choose", "func Init() Info {", false},
		// Right ones.
		{"- **`Order.Validate`** ([types.go:292](types.go#L292)). Field checks", "func (o Order) Validate() RejectReason {", true},
		{"**`rest(o, qty)`** ([book.go:290](book.go#L290)):", "func (b *Book) rest(o *Order, qty Qty) {", true},
		{"**`fsutil.SyncDir`** ([fsutil.go:11](fsutil.go#L11))", "func SyncDir(dir string) error {", true},
		{"**`Book.chunks`** ([book.go:20](book.go#L20))", "type Book struct {", true},
		{"**`Book.Submit`** ([book.go:20](book.go#L20))", "func (b *Book) Submit(o Order, fills []Fill) (Result, []Fill) {", true},
	}
	for _, c := range cases {
		i := strings.LastIndex(c.doc, "([")
		got := declaresAny([]string{c.decl}, 0, namedBefore(c.doc, i+1))
		if got != c.ok {
			t.Errorf("%q on %q: accepted %v, want %v", c.doc, c.decl, got, c.ok)
		}
	}
}
