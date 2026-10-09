package doctor

import (
	"bytes"
	"strings"
	"testing"
)

func FuzzLiteralDirectInclude(f *testing.F) {
	for _, seed := range []string{"", "include " + doctorSnippetName + "\n", "include $source\n", "mode default {\ninclude " + doctorSnippetName + "\n}\n", "exec sh -c '{'\n", "include \\\n " + doctorSnippetName + "\n", "\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > maxSwayConfigBytes {
			t.Skip()
		}
		got := literalDirectInclude([]byte(source), "/fixture/config", "/fixture/"+doctorSnippetName)
		if got != literalDirectInclude([]byte(source), "/fixture/config", "/fixture/"+doctorSnippetName) || got < 0 || got > strings.Count(source, "\n")+1 {
			t.Fatal("inconsistent or invalid source location")
		}
	})
}

func FuzzManagedSnippetExactOwnership(f *testing.F) {
	for _, kinds := range [][]integrationKind{nil, {integrationDaemon}, {integrationDaemon, integrationRestore}, integrationOrder} {
		f.Add(renderManagedSnippet("/usr/bin/sway-session", kinds))
	}
	f.Add([]byte(doctorHeader + "exec sh -c 'sway-session daemon'\n"))
	f.Fuzz(func(t *testing.T, content []byte) {
		if len(content) > maxSwayConfigBytes {
			t.Skip()
		}
		parsed, err := parseManagedSnippet(content)
		if err != nil {
			return
		}
		kinds := []integrationKind{integrationDaemon, integrationRestore}
		if parsed.shortcuts == ShortcutsDefault {
			kinds = integrationOrder
		} else if parsed.shortcuts != ShortcutsNone {
			t.Fatal("accepted an unknown profile")
		}
		if !bytes.Equal(content, renderManagedSnippet(parsed.executable, kinds)) {
			t.Fatal("accepted manually edited standard content")
		}
	})
}
