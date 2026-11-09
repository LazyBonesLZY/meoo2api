package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestSelfContainedFiveViews(t *testing.T) {
	page := Index()
	if bytes.Contains(page, []byte("/*__CONSOLE_SCRIPT__*/")) || !bytes.Contains(page, script) {
		t.Fatal("script was not embedded")
	}
	if got := strings.Count(string(page), `<section class="view`); got != 5 {
		t.Fatalf("got %d views, want 5", got)
	}
	for _, name := range []string{"overview", "accounts", "apikeys", "logs", "settings"} {
		if !bytes.Contains(page, []byte(`id="v-`+name+`"`)) {
			t.Fatalf("missing %s page", name)
		}
	}
	page[0] = 'x'
	if Index()[0] != '<' {
		t.Fatal("Index exposes mutable shared asset")
	}
	if bytes.Contains(script, []byte("</script")) {
		t.Fatal("embedded JS closes script context")
	}
}
func TestConsoleJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node runtime not installed")
	}
	for _, args := range [][]string{{"--check", "console.js"}, {"--test", "console.test.cjs"}} {
		if out, err := exec.Command(node, args...).CombinedOutput(); err != nil {
			t.Fatalf("node %v: %v\n%s", args, err, out)
		}
	}
}
