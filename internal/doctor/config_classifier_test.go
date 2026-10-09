package doctor

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLiteralDirectIncludeRecognizesOnlyTopLevelLiteralSibling(t *testing.T) {
	main := "/fixture/config"
	sibling := "/fixture/" + doctorSnippetName
	for _, test := range []struct {
		name, source string
		line         int
	}{
		{"relative", "include " + doctorSnippetName + "\n", 1},
		{"absolute quoted", "# comment\ninclude \"" + sibling + "\"\n", 2},
		{"uppercase keyword", "INCLUDE " + doctorSnippetName + "\n", 1},
		{"first evidence", "include " + doctorSnippetName + "\ninclude " + doctorSnippetName + "\n", 1},
		{"comment", "# include " + doctorSnippetName + "\n", 0},
		{"inline comment", "include " + doctorSnippetName + " # note\n", 0},
		{"variable", "set $source " + sibling + "\ninclude $source\n", 0},
		{"glob", "include *.conf\n", 0},
		{"parent traversal", "include ../fixture/" + doctorSnippetName + "\n", 0},
		{"normalized relative", "include ./" + doctorSnippetName + "\n", 1},
		{"shell substitution", "include $(printf '" + sibling + "')\n", 0},
		{"nested block", "mode default {\n include " + doctorSnippetName + "\n}\n", 0},
		{"continued include", "include \\\n " + doctorSnippetName + "\n", 0},
		{"continued payload", "exec notify-send \\\n include " + doctorSnippetName + "\n", 0},
		{"startup payload", "exec include " + doctorSnippetName + "\n", 0},
		{"startup prefix block", "exec {\n include " + doctorSnippetName + "\n}\n", 0},
		{"startup option prefix block", "exec --no-startup-id {\n include " + doctorSnippetName + "\n}\n", 0},
		{"startup command prefix block", "exec /usr/bin/true {\n include " + doctorSnippetName + "\n}\n", 0},
		{"startup shell prefix block", "exec sh -c true {\n include " + doctorSnippetName + "\n}\n", 0},
		{"startup always option prefix block", "exec_always --no-startup-id {\n include " + doctorSnippetName + "\n}\n", 0},
		{"startup always prefix block", "exec_always {\n include " + doctorSnippetName + "\n}\n", 0},
		{"opaque startup braces", "exec sh -c '{'\nexec_always sh -c '{'\ninclude " + doctorSnippetName + "\n", 3},
		{"foreign include", "include foreign.conf\n", 0},
		{"quoted trailing payload", "include \"" + sibling + "\" exec true\n", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := literalDirectInclude([]byte(test.source), main, sibling); got != test.line {
				t.Fatalf("include line=%d, want %d for %q", got, test.line, test.source)
			}
		})
	}
}

func TestLiteralIncludePathRejectsExpansionAndNonLiteralSyntax(t *testing.T) {
	for _, argument := range []string{"", "$HOME/file", "~/../file", "file*", "file?", "file[12]", "`true`", "file\x00", "\"unterminated", "'file'", "a b", "\"file\" trailing", "\"file\\n\"", "../file"} {
		if path, ok := literalIncludePath(argument); ok {
			t.Errorf("accepted nonliteral argument %q as %q", argument, path)
		}
	}
	for _, argument := range []string{"file.conf", "./file.conf", "a//file.conf", "parts#colors.conf", "\"directory with spaces/file.conf\"", `"directory\\name/file.conf"`, `"directory\"name/file.conf"`} {
		path, ok := literalIncludePath(argument)
		if !ok || filepath.Clean(path) != path {
			t.Errorf("rejected literal argument %q: %q", argument, path)
		}
	}
}

func TestLiteralDirectIncludeBoundsLinesAndBlockNesting(t *testing.T) {
	const main = "/fixture/config"
	sibling := "/fixture/" + doctorSnippetName
	directive := "include " + doctorSnippetName + "\n"
	for _, test := range []struct {
		name, source string
		line         int
	}{
		{"oversized line", "include " + strings.Repeat(" ", maxSwayConfigLine) + doctorSnippetName + "\n" + directive, 0},
		{"oversized continued line", strings.Repeat("x", maxSwayConfigLine) + "\\\n" + directive + directive, 0},
		{"oversized block opener", "exec /usr/bin/true " + strings.Repeat(" ", maxSwayConfigLine) + "{\n" + directive + "}\n", 0},
		{"oversized continued block opener", "exec /usr/bin/true \\\n" + strings.Repeat(" ", maxSwayConfigLine) + "{\n" + directive + "}\n", 0},
		{"earlier evidence before oversized line", directive + strings.Repeat("x", maxSwayConfigLine+1) + "\n" + directive, 1},
		{"deep blocks", strings.Repeat("mode default {\n", maxSwayBlockDepth+1) + directive, 0},
		{"earlier evidence retained", directive + strings.Repeat("mode default {\n", maxSwayBlockDepth+1), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := literalDirectInclude([]byte(test.source), main, sibling); got != test.line {
				t.Fatalf("include line=%d, want %d", got, test.line)
			}
		})
	}
}

func TestLiteralDirectIncludeCommentContinuationBoundaries(t *testing.T) {
	const main = "/fixture/config"
	sibling := "/fixture/" + doctorSnippetName
	directive := "include " + doctorSnippetName + "\n"
	for _, test := range []struct {
		name, source string
		line         int
		appendSafe   bool
	}{
		{"column zero comment", "# note \\\n" + directive, 2, true},
		{"indented comment", "  # note \\\n" + directive, 0, true},
		{"tab indented comment", "\t# note \\\n" + directive, 0, true},
		{"indented comment at EOF", "  # note \\\n", 0, false},
		{"indented comment without newline", "  # note \\", 0, false},
		{"blank ends indented comment", "  # note \\\n\n" + directive, 3, true},
		{"comment within continued command", "exec /usr/bin/true \\\n# note \\\n" + directive, 0, true},
		{"continued comment at EOF", "exec /usr/bin/true \\\n# note \\\n", 0, false},
		{"comment after empty continuation", "\\\n# note \\\n" + directive, 3, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			line, err := inspectDirectInclude([]byte(test.source), main, sibling)
			if line != test.line || (err == nil) != test.appendSafe {
				t.Fatalf("line=%d error=%v, want line=%d appendSafe=%t", line, err, test.line, test.appendSafe)
			}
		})
	}
}
