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
		for _, m := range codeRe.FindAllStringSubmatch(string(data), -1) {
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
