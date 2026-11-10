//go:build linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const version = "0.2.0"
const outputLimit = 2 << 20

type Issue struct {
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Message string `json:"message"`
}
type Section struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Source     string    `json:"source"`
	Scope      string    `json:"scope"`
	Started    time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	Data       any       `json:"data,omitempty"`
	Issues     []Issue   `json:"issues"`
	Notes      []string  `json:"notes,omitempty"`
}
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Version       string    `json:"version"`
	Started       time.Time `json:"started_at"`
	DurationMS    int64     `json:"duration_ms"`
	EUID          int       `json:"euid"`
	Sections      []Section `json:"sections"`
}
type collector struct {
	name, source, scope string
	collect             func(context.Context, *Section)
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("srvpeek", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "output JSON (schema_version=1)")
	timeout := fs.Duration("timeout", 4*time.Second, "timeout per section")
	full := fs.Bool("full", false, "show all collected data")
	save := fs.String("save", "", "save JSON to a new file (permissions 0600)")
	color := fs.String("color", "auto", "color mode: auto, always, never (respects NO_COLOR in auto mode)")
	showVersion := fs.Bool("version", false, "version")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "srvpeek [flags] [system|disks|network|ports|services|timers|cron|containers|firewall]\nRead-only; Linux. Status ok refers to collection, not system health.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *color != "auto" && *color != "always" && *color != "never" {
		fmt.Fprintln(errOut, "--color must be auto, always or never")
		return 2
	}
	if *showVersion {
		fmt.Fprintln(out, version)
		return 0
	}
	if *timeout <= 0 || *timeout > time.Minute {
		fmt.Fprintln(errOut, "--timeout must be > 0 and <= 1m")
		return 2
	}
	cs := collectors()
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if fs.NArg() == 1 {
		found := false
		for _, c := range cs {
			if c.name == fs.Arg(0) {
				cs = []collector{c}
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintln(errOut, "unknown section:", fs.Arg(0))
			return 2
		}
		*full = true
	}
	report := Report{SchemaVersion: 1, Version: version, Started: time.Now().UTC(), EUID: os.Geteuid(), Sections: make([]Section, len(cs))}
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func(i int, c collector) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()
			// A separate result channel also bounds reads of pseudo files or network mounts.
			result := make(chan Section, 1)
			go func() {
				s := Section{Name: c.name, Source: c.source, Scope: c.scope, Status: "ok", Started: time.Now().UTC(), Issues: []Issue{}}
				c.collect(ctx, &s)
				if len(s.Issues) > 0 {
					if s.Data != nil {
						s.Status = "partial"
					} else {
						s.Status = s.Issues[0].Kind
					}
				}
				s.DurationMS = time.Since(s.Started).Milliseconds()
				result <- s
			}()
			select {
			case s := <-result:
				report.Sections[i] = s
			case <-ctx.Done():
				report.Sections[i] = Section{Name: c.name, Source: c.source, Scope: c.scope, Status: "timeout", Started: report.Started, DurationMS: timeout.Milliseconds(), Issues: []Issue{{"timeout", c.source, "Section timed out; data completeness is unknown"}}}
			}
		}(i, c)
	}
	wg.Wait()
	report.DurationMS = time.Since(report.Started).Milliseconds()
	if *save != "" {
		f, err := os.OpenFile(*save, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			fmt.Fprintln(errOut, "save:", err)
			return 2
		}
		err = json.NewEncoder(f).Encode(report)
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			fmt.Fprintln(errOut, "snapshot write error:", err, closeErr)
			return 2
		}
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintln(errOut, err)
			return 2
		}
	} else {
		renderWithOptions(out, report, *full, outputOptions(out, *color))
	}
	// Missing telemetry is not a health verdict. Exit 1 means incomplete collection.
	for _, s := range report.Sections {
		if s.Status != "ok" {
			return 1
		}
	}
	return 0
}

func issue(s *Section, kind, source, message string) {
	s.Issues = append(s.Issues, Issue{kind, source, message})
}
func file(s *Section, path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		kind := "error"
		if os.IsPermission(err) {
			kind = "denied"
		}
		if os.IsNotExist(err) {
			kind = "unavailable"
		}
		issue(s, kind, path, err.Error())
		return nil, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, outputLimit+1))
	if err != nil {
		issue(s, "error", path, err.Error())
		return nil, false
	}
	if len(b) > outputLimit {
		issue(s, "truncated", path, "File exceeds the 2 MiB limit; data discarded")
		return nil, false
	}
	return b, true
}

type boundedBuffer struct {
	b         []byte
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := outputLimit - len(b.b)
	if len(p) > left {
		b.truncated = true
		p = p[:left]
	}
	b.b = append(b.b, p...)
	return n, nil
}
func command(ctx context.Context, s *Section, name string, args ...string) (string, bool) {
	source := name + " " + strings.Join(args, " ")
	path, err := exec.LookPath(name)
	if err != nil {
		issue(s, "missing_tool", source, "Command not found")
		return "", false
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 200 * time.Millisecond
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=cat")
	var stdout, stderr boundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		issue(s, "timeout", source, "Command timed out; its output was discarded")
		return "", false
	}
	if err != nil {
		issue(s, "error", source, fmt.Sprintf("%v; stderr: %s", err, strings.TrimSpace(string(stderr.b))))
		return "", false
	}
	if stdout.truncated || stderr.truncated {
		issue(s, "truncated", source, "Output exceeded the 2 MiB limit; data discarded")
		return "", false
	}
	if strings.TrimSpace(string(stderr.b)) != "" {
		issue(s, "warning", source, "Command exited with code 0 but wrote to stderr; stdout is not interpreted: "+strings.TrimSpace(string(stderr.b)))
		return "", false
	}
	return string(stdout.b), true
}
func jsonCommand(ctx context.Context, s *Section, name string, args ...string) (any, bool) {
	raw, ok := command(ctx, s, name, args...)
	if !ok {
		return nil, false
	}
	var data any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		issue(s, "parse_error", name, err.Error())
		return nil, false
	}
	if _, ok := data.([]any); !ok {
		issue(s, "parse_error", name, "Expected a JSON array")
		return nil, false
	}
	return data, true
}
func nonemptyLines(raw string) []string {
	out := []string{}
	for _, l := range strings.Split(raw, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return ' '
		}
		return r
	}, s)
}

func collectors() []collector {
	return []collector{
		{"system", "/proc, /etc/os-release", "Local OS and procfs; load is not CPU utilization", collectSystem},
		{"disks", "/proc/self/mountinfo, statfs", "Visible local filesystems; network and pseudo-filesystems excluded", collectDisks},
		{"network", "ip -j, /etc/resolv.conf", "Current network namespace; resolv.conf does not describe all split-DNS rules", collectNetwork},
		{"ports", "ss -H -lntup", "Listening TCP and unconnected UDP sockets in the current network namespace", collectPorts},
		{"services", "systemctl list-units / list-unit-files", "System systemd manager: loaded service units and installed unit files; user managers excluded", collectServices},
		{"timers", "systemctl list-timers", "System systemd timers; user managers excluded", collectTimers},
		{"cron", "/etc/crontab, /etc/cron.*, cron spool", "Known local cron paths; jobs may contain sensitive arguments", collectCron},
		{"containers", "docker context, docker ps, docker inspect", "Docker in the selected CLI context; Podman/CRI/Kubernetes not checked", collectContainers},
		{"firewall", "nft list ruleset", "Only nftables in the current network namespace; external ACLs and legacy iptables not checked", collectFirewall},
	}
}
