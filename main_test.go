//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"
)

func TestSocketUnknownIsNotEmpty(t *testing.T) {
	rows, err := parseSockets("tcp LISTEN 0 128 0.0.0.0:22 0.0.0.0:*\nudp UNCONN 0 0 [::]:53 [::]:* users:((\"dns\",pid=123,fd=4))\n")
	if err != nil || len(rows) != 2 || rows[0].OwnerKnown || !rows[1].OwnerKnown {
		t.Fatalf("unexpected: %+v %v", rows, err)
	}
	empty, err := parseSockets("")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal("successful empty must be explicit empty array")
	}
	for _, raw := range []string{"Netid State Recv-Q Send-Q Local Peer", "garbage", "tcp LISTEN x 0 *:22 *:*", "tcp MYSTERY 0 0 *:22 *:*", "tcp LISTEN 0 0 *:22 *:* unknown-extension"} {
		if _, err := parseSockets(raw); err == nil {
			t.Fatalf("accepted malformed ss: %q", raw)
		}
	}
}
func TestMemoryRejectsIncompleteAndWrongUnits(t *testing.T) {
	good := "MemTotal: 100 kB\nMemAvailable: 25 kB\nSwapTotal: 10 kB\nSwapFree: 5 kB\n"
	m, err := parseMeminfo(good)
	if err != nil || m["MemTotal"] != 102400 {
		t.Fatal(m, err)
	}
	for _, raw := range []string{"", strings.Replace(good, " kB", " MB", 1), strings.Replace(good, "MemAvailable: 25 kB\n", "", 1), good + "MemTotal: 2 kB\n", strings.Replace(good, "SwapFree: 5", "SwapFree: 50", 1)} {
		if _, err := parseMeminfo(raw); err == nil {
			t.Fatalf("accepted invalid meminfo %q", raw)
		}
	}
}
func TestIPRejectsUnexpectedShape(t *testing.T) {
	for _, raw := range []string{`[{}]`, `["not interface"]`, `[{"ifname":"eth0","ifindex":1}]`, `[{"ifname":"eth0","ifindex":1,"addr_info":[{}]}]`} {
		var v any
		json.Unmarshal([]byte(raw), &v)
		if err := validateIP(v, "interfaces"); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	var good any
	json.Unmarshal([]byte(`[{"ifname":"lo","ifindex":1,"addr_info":[]}]`), &good)
	if err := validateIP(good, "interfaces"); err != nil {
		t.Fatal(err)
	}
}
func TestServicesPreserveInactive(t *testing.T) {
	rows, err := parseServices("backup.service loaded inactive dead Backup job\n")
	if err != nil || len(rows) != 1 || rows[0].Active != "inactive" {
		t.Fatal(rows, err)
	}
	if _, err := parseServices("unrecognized header"); err == nil {
		t.Fatal("malformed services accepted")
	}
}

func TestDockerProjectionOmitsSecretsAndAllowsMissingHealth(t *testing.T) {
	id := strings.Repeat("a", 64)
	input := map[string]any{"Id": id, "Name": "/example", "Config": map[string]any{"Image": "test:1", "Env": []string{"PASSWORD=do-not-copy"}, "Labels": map[string]string{}}, "State": map[string]any{"Status": "exited", "Running": false, "ExitCode": 0, "StartedAt": "t", "FinishedAt": "t"}, "RestartCount": 0, "HostConfig": map[string]any{"NetworkMode": "bridge", "RestartPolicy": map[string]any{"Name": "no"}}, "NetworkSettings": map[string]any{"Networks": map[string]any{}, "Ports": map[string]any{}}, "Mounts": []any{}}
	tpl, err := template.New("inspect").Funcs(template.FuncMap{"json": func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return string(b)
	}}).Parse(inspectTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := tpl.Execute(&b, input); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "PASSWORD") || strings.Contains(b.String(), "do-not-copy") {
		t.Fatal("secret leaked")
	}
	c, err := parseContainer(b.String(), id)
	if err != nil || c.State.Health != nil || c.State.Running == nil || *c.State.Running || c.State.ExitCode == nil || *c.State.ExitCode != 0 {
		t.Fatalf("%s: %+v %v", b.String(), c, err)
	}
	for _, raw := range []string{`{}`, `null`, `{"Id":"wrong"}`, `not json`} {
		if _, err := parseContainer(raw, id); err == nil {
			t.Fatal("accepted malformed container", raw)
		}
	}
}

// Real child processes verify that a failed/truncated/timed-out command cannot look like an empty successful result.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("SRVPEEK_TEST_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "empty":
	case "fail":
		fmt.Fprint(os.Stdout, "tcp LISTEN 0 0 *:22 *:*\n")
		fmt.Fprint(os.Stderr, "access denied")
		os.Exit(7)
	case "sleep":
		time.Sleep(5 * time.Second)
	case "large":
		fmt.Print(strings.Repeat("x", outputLimit+1))
	case "warning":
		fmt.Fprint(os.Stdout, "[]")
		fmt.Fprint(os.Stderr, "limited view")
	}
	os.Exit(0)
}
func TestCommandFailureModes(t *testing.T) {
	t.Setenv("SRVPEEK_TEST_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode, kind string
		ok         bool
	}{{"empty", "", true}, {"fail", "error", false}, {"sleep", "timeout", false}, {"large", "truncated", false}, {"warning", "warning", false}} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			s := Section{}
			raw, ok := command(ctx, &s, exe, "-test.run=^TestHelperProcess$", "--", tc.mode)
			if ok != tc.ok {
				t.Fatalf("ok=%v issues=%+v", ok, s.Issues)
			}
			if !ok && raw != "" {
				t.Fatal("failure returned usable output")
			}
			if tc.kind != "" && (len(s.Issues) == 0 || s.Issues[0].Kind != tc.kind) {
				t.Fatalf("wanted %s got %+v", tc.kind, s.Issues)
			}
		})
	}
	s := Section{}
	_, ok := command(context.Background(), &s, "srvpeek-nonexistent-test-command-1984")
	if ok || s.Issues[0].Kind != "missing_tool" {
		t.Fatal(s)
	}
}
func TestCLIRejectsBadArguments(t *testing.T) {
	for _, args := range [][]string{{"--timeout=0"}, {"--timeout=2m"}, {"missing"}, {"ports", "services"}} {
		var out, err bytes.Buffer
		if code := run(args, &out, &err); code != 2 {
			t.Fatal(args, code)
		}
	}
}
func TestIncompleteReportAndSavePermissions(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out, err bytes.Buffer
	path := filepath.Join(t.TempDir(), "report.json")
	code := run([]string{"--json", "--save", path, "ports"}, &out, &err)
	if code != 1 {
		t.Fatalf("expected incomplete exit 1, got %d: %s", code, err.String())
	}
	var r Report
	if e := json.Unmarshal(out.Bytes(), &r); e != nil {
		t.Fatal(e)
	}
	if len(r.Sections) != 1 || r.Sections[0].Status != "missing_tool" || r.Sections[0].Data != nil {
		t.Fatalf("missing ss must not mean no sockets: %+v", r)
	}
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, e)
	}
	out.Reset()
	err.Reset()
	if c := run([]string{"--save", path, "ports"}, &out, &err); c != 2 {
		t.Fatal("existing report overwritten")
	}
}
func TestTerminalControlCharactersRemoved(t *testing.T) {
	if got := clean("ok\x1b[31m\n\t\u202eevil"); strings.ContainsAny(got, "\x1b\n\t\u202e") {
		t.Fatal("unsafe", got)
	}
}

func TestSuccessfulSSWithPermissionWarningIsNotNoPorts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ss"), []byte("#!/bin/sh\necho 'Cannot open netlink socket: Operation not permitted' >&2\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var out, errOut bytes.Buffer
	if code := run([]string{"--json", "ports"}, &out, &errOut); code != 1 {
		t.Fatalf("code=%d: %s", code, errOut.String())
	}
	var r Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	s := r.Sections[0]
	if s.Status != "warning" || s.Data != nil {
		t.Fatalf("ss warning became usable socket data: %+v", s)
	}
	out.Reset()
	render(&out, r, false)
	if strings.Contains(out.String(), "No records in the received response") {
		t.Fatal("unknown sockets presented as empty")
	}
}
