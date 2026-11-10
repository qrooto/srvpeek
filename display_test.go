//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestTableWrapsWithoutTruncatingAndFitsWidth(t *testing.T) {
	for _, width := range []int{40, 80, 100, 140} {
		var b bytes.Buffer
		w := &displayWriter{&b, displayOptions{width: width}}
		table(w, "NAME\tSTATE\tADDRESS\tDETAILS", []string{"api\trunning\t[::]:443\t" + strings.Repeat("x", 181)}, true, "ports")
		if strings.Count(b.String(), "x") != 181 {
			t.Fatal("cell content lost", b.String())
		}
		for _, line := range strings.Split(b.String(), "\n") {
			if displayWidth(line) > width {
				t.Fatalf("width %d overflow %d: %s", width, displayWidth(line), line)
			}
		}
	}
}
func TestColorDoesNotChangeAlignmentOrSemantics(t *testing.T) {
	r := Report{Version: version, Sections: []Section{{Name: "ports", Status: "timeout", Issues: []Issue{{"timeout", "ss", "No socket inventory obtained"}}}}}
	var plain, colored bytes.Buffer
	renderWithOptions(&plain, r, false, displayOptions{width: 80})
	renderWithOptions(&colored, r, false, displayOptions{width: 80, color: true})
	strip := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	if !strings.Contains(colored.String(), "\x1b[") {
		t.Fatal("missing color")
	}
	if strip.ReplaceAllString(colored.String(), "") != plain.String() {
		t.Fatal("color changed report contents")
	}
	if !strings.Contains(plain.String(), "Data unavailable") || strings.Contains(plain.String(), "No records in the received response") {
		t.Fatal("timeout presented as empty inventory")
	}
}
func TestColorModesAndJSON(t *testing.T) {
	var b bytes.Buffer
	if outputOptions(&b, "auto").color {
		t.Fatal("buffer must not get automatic ANSI")
	}
	t.Setenv("NO_COLOR", "")
	if outputOptions(&b, "auto").color || !outputOptions(&b, "always").color || outputOptions(os.Stdout, "never").color {
		t.Fatal("color preference ignored")
	}
	t.Setenv("PATH", t.TempDir())
	var errOut bytes.Buffer
	if code := run([]string{"--color=always", "--json", "ports"}, &b, &errOut); code != 1 {
		t.Fatal(code, errOut.String())
	}
	if strings.Contains(b.String(), "\x1b") || !json.Valid(b.Bytes()) {
		t.Fatal("JSON contaminated by terminal formatting")
	}
	b.Reset()
	errOut.Reset()
	if run([]string{"--color=invalid"}, &b, &errOut) != 2 {
		t.Fatal("invalid color mode accepted")
	}
}
func TestPortBindingDisplayDoesNotInventBindings(t *testing.T) {
	text := portBindingsText(map[string]json.RawMessage{"443/tcp": json.RawMessage(`[{"HostIp":"::","HostPort":"8443"}]`), "80/tcp": json.RawMessage(`null`)})
	if !strings.Contains(text, "[::]:8443 → 443/tcp") || !strings.Contains(text, "80/tcp (no host binding reported)") {
		t.Fatal(text)
	}
}
