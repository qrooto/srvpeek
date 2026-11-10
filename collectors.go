//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func collectSystem(_ context.Context, s *Section) {
	d := map[string]any{"architecture": runtime.GOARCH, "logical_cpus_visible": runtime.NumCPU()}
	s.Data = d
	if h, err := os.Hostname(); err == nil {
		d["hostname"] = h
	} else {
		issue(s, "error", "hostname", err.Error())
	}
	if b, ok := file(s, "/etc/os-release"); ok {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "PRETTY_NAME=") {
				d["os"] = strings.Trim(strings.TrimPrefix(l, "PRETTY_NAME="), "\"'")
			}
		}
		if d["os"] == nil {
			issue(s, "parse_error", "/etc/os-release", "Missing PRETTY_NAME")
		}
	}
	if b, ok := file(s, "/proc/sys/kernel/osrelease"); ok {
		d["kernel"] = strings.TrimSpace(string(b))
	}
	if b, ok := file(s, "/proc/uptime"); ok {
		fields := strings.Fields(string(b))
		if len(fields) > 0 {
			if n, err := strconv.ParseFloat(fields[0], 64); err == nil && n >= 0 && !math.IsInf(n, 0) {
				d["uptime_seconds"] = n
			} else {
				issue(s, "parse_error", "/proc/uptime", "Invalid uptime")
			}
		} else {
			issue(s, "parse_error", "/proc/uptime", "Empty response")
		}
	}
	if b, ok := file(s, "/proc/loadavg"); ok {
		f := strings.Fields(string(b))
		if len(f) >= 3 {
			v := []float64{}
			for _, x := range f[:3] {
				n, err := strconv.ParseFloat(x, 64)
				if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
					issue(s, "parse_error", "/proc/loadavg", "Invalid load average")
					break
				}
				v = append(v, n)
			}
			if len(v) == 3 {
				d["load_1_5_15"] = v
			}
		} else {
			issue(s, "parse_error", "/proc/loadavg", "Expected three values")
		}
	}
	if b, ok := file(s, "/proc/meminfo"); ok {
		m, err := parseMeminfo(string(b))
		if err != nil {
			issue(s, "parse_error", "/proc/meminfo", err.Error())
		} else {
			d["memory_bytes"] = m
			if m["MemAvailable"] <= m["MemTotal"] {
				d["memory_used_estimate_bytes"] = m["MemTotal"] - m["MemAvailable"]
			} else {
				issue(s, "parse_error", "/proc/meminfo", "MemAvailable exceeds MemTotal")
			}
		}
	}
	s.Notes = []string{"CPU: logical CPUs available to this process. Memory and load from procfs may describe the host rather than cgroup limits. CPU utilization and application health are not measured."}
}
func parseMeminfo(raw string) (map[string]uint64, error) {
	wanted := map[string]bool{"MemTotal": true, "MemAvailable": true, "SwapTotal": true, "SwapFree": true}
	m := map[string]uint64{}
	for _, l := range strings.Split(raw, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		key := strings.TrimSuffix(f[0], ":")
		if !wanted[key] {
			continue
		}
		if len(f) != 3 || f[2] != "kB" {
			return nil, fmt.Errorf("unexpected format for %s", key)
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil || n > ^uint64(0)/1024 {
			return nil, fmt.Errorf("invalid number for %s", key)
		}
		if _, ok := m[key]; ok {
			return nil, fmt.Errorf("duplicate field %s", key)
		}
		m[key] = n * 1024
	}
	for k := range wanted {
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("missing field %s; memory estimate unavailable", k)
		}
	}
	if m["SwapFree"] > m["SwapTotal"] {
		return nil, fmt.Errorf("SwapFree exceeds SwapTotal")
	}
	return m, nil
}

type disk struct {
	Mount             string   `json:"mount"`
	Device            string   `json:"device"`
	FS                string   `json:"fs"`
	Total             uint64   `json:"total_bytes"`
	Available         uint64   `json:"available_to_unprivileged_bytes"`
	UsedPercent       *float64 `json:"blocks_used_percent,omitempty"`
	InodesUsedPercent *float64 `json:"inodes_used_percent,omitempty"`
	ReadOnly          bool     `json:"read_only"`
}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
func collectDisks(ctx context.Context, s *Section) {
	b, ok := file(s, "/proc/self/mountinfo")
	if !ok {
		return
	}
	rows := []disk{}
	excluded := map[string]int{}
	local := map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "overlay": true, "f2fs": true, "vfat": true, "exfat": true, "bcachefs": true}
	for _, l := range nonemptyLines(string(b)) {
		if ctx.Err() != nil {
			return
		}
		halves := strings.SplitN(l, " - ", 2)
		if len(halves) != 2 {
			issue(s, "parse_error", "mountinfo", "Missing record separator")
			continue
		}
		a, z := strings.Fields(halves[0]), strings.Fields(halves[1])
		if len(a) < 6 || len(z) < 3 {
			issue(s, "parse_error", "mountinfo", "Incomplete record")
			continue
		}
		if !local[z[0]] {
			excluded[z[0]]++
			continue
		}
		mount := unescapeMount(a[4])
		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil {
			issue(s, "error", mount, err.Error())
			continue
		}
		if st.Bsize <= 0 || st.Bfree > st.Blocks || st.Bavail > st.Blocks || st.Blocks > ^uint64(0)/uint64(st.Bsize) {
			issue(s, "parse_error", mount, "Invalid statfs counters")
			continue
		}
		row := disk{Mount: mount, Device: unescapeMount(z[1]), FS: z[0], Total: st.Blocks * uint64(st.Bsize), Available: st.Bavail * uint64(st.Bsize), ReadOnly: st.Flags&1 != 0}
		if st.Blocks > 0 {
			v := float64(st.Blocks-st.Bfree) / float64(st.Blocks) * 100
			row.UsedPercent = &v
		}
		if st.Files > 0 && st.Ffree <= st.Files {
			v := float64(st.Files-st.Ffree) / float64(st.Files) * 100
			row.InodesUsedPercent = &v
		}
		rows = append(rows, row)
	}
	s.Data = map[string]any{"filesystems": rows, "excluded_mounts_by_fs": excluded}
	s.Notes = []string{"Bind mounts and overlay may report the same storage multiple times; do not sum their capacities. blocks_used_percent uses Blocks/Bfree; available space uses Bavail (excluding the root reserve)."}
}
func collectNetwork(ctx context.Context, s *Section) {
	d := map[string]any{}
	for _, q := range []struct {
		key  string
		args []string
	}{{"interfaces", []string{"-j", "address", "show"}}, {"routes_ipv4", []string{"-j", "-4", "route", "show", "table", "all"}}, {"routes_ipv6", []string{"-j", "-6", "route", "show", "table", "all"}}} {
		if v, ok := jsonCommand(ctx, s, "ip", q.args...); ok {
			if err := validateIP(v, q.key); err != nil {
				issue(s, "parse_error", "ip "+q.key, err.Error())
			} else {
				d[q.key] = v
			}
		}
	}
	if b, ok := file(s, "/etc/resolv.conf"); ok {
		lines := []string{}
		for _, l := range nonemptyLines(string(b)) {
			l = strings.TrimSpace(l)
			if !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, ";") {
				lines = append(lines, l)
			}
		}
		d["resolv_conf_directives"] = lines
	}
	if len(d) > 0 {
		s.Data = d
	}
	s.Notes = []string{"An interface address and route do not confirm internet connectivity. No DNS queries or requests to external addresses were made."}
}
func validateIP(v any, key string) error {
	rows, ok := v.([]any)
	if !ok {
		return fmt.Errorf("expected an array")
	}
	for i, v := range rows {
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("record %d is not an object", i)
		}
		if key == "interfaces" {
			if _, ok := m["ifname"].(string); !ok {
				return fmt.Errorf("record %d: missing ifname", i)
			}
			if _, ok := m["ifindex"].(float64); !ok {
				return fmt.Errorf("record %d: missing ifindex", i)
			}
			a, ok := m["addr_info"].([]any)
			if !ok {
				return fmt.Errorf("record %d: missing addr_info", i)
			}
			for _, v := range a {
				a, ok := v.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid addr_info")
				}
				if _, ok := a["local"].(string); !ok {
					return fmt.Errorf("missing local address")
				}
				if _, ok := a["prefixlen"].(float64); !ok {
					return fmt.Errorf("missing prefixlen")
				}
			}
		} else {
			if _, ok := m["dst"].(string); !ok {
				return fmt.Errorf("record %d: missing dst", i)
			}
		}
	}
	return nil
}

type process struct {
	PID                     int      `json:"pid"`
	Executable              string   `json:"executable,omitempty"`
	CgroupServiceCandidates []string `json:"service_candidates_from_cgroup,omitempty"`
}
type socket struct {
	Protocol   string    `json:"protocol"`
	State      string    `json:"state"`
	Local      string    `json:"local"`
	Peer       string    `json:"peer"`
	Owner      string    `json:"owner_reported,omitempty"`
	OwnerKnown bool      `json:"owner_reported_present"`
	Processes  []process `json:"processes,omitempty"`
}

var pidPattern = regexp.MustCompile(`pid=(\d+)`)

func enrichSockets(ctx context.Context, s *Section, rows []socket) {
	cache := map[int]process{}
	for i := range rows {
		for _, match := range pidPattern.FindAllStringSubmatch(rows[i].Owner, -1) {
			if ctx.Err() != nil {
				return
			}
			pid, err := strconv.Atoi(match[1])
			if err != nil || pid < 1 {
				issue(s, "parse_error", "ss", "Invalid PID")
				continue
			}
			p, ok := cache[pid]
			if !ok {
				if len(cache) >= 500 {
					issue(s, "truncated", "/proc", "Socket-to-process lookup is limited to 500 PIDs")
					return
				}
				p = process{PID: pid}
				path := fmt.Sprintf("/proc/%d", pid)
				if exe, err := os.Readlink(path + "/exe"); err == nil {
					p.Executable = exe
				} else {
					issue(s, "unavailable", path+"/exe", err.Error())
				}
				if b, ok := file(s, path+"/cgroup"); ok {
					units := map[string]bool{}
					for _, l := range nonemptyLines(string(b)) {
						f := strings.SplitN(l, ":", 3)
						if len(f) != 3 {
							issue(s, "parse_error", path+"/cgroup", "Invalid record")
							continue
						}
						for _, part := range strings.Split(f[2], "/") {
							if strings.HasSuffix(part, ".service") {
								units[part] = true
							}
						}
					}
					p.CgroupServiceCandidates = sortedKeys(units)
				}
				cache[pid] = p
			}
			rows[i].Processes = append(rows[i].Processes, p)
		}
	}
}
func parseSockets(raw string) ([]socket, error) {
	rows := []socket{}
	for n, l := range nonemptyLines(raw) {
		f := strings.Fields(l)
		if len(f) < 6 {
			return nil, fmt.Errorf("line %d: expected at least 6 fields", n+1)
		}
		if (f[0] != "tcp" && f[0] != "udp") || (f[1] != "LISTEN" && f[1] != "UNCONN") {
			return nil, fmt.Errorf("line %d: unknown protocol or state", n+1)
		}
		for _, q := range f[2:4] {
			if _, err := strconv.ParseUint(q, 10, 64); err != nil {
				return nil, fmt.Errorf("line %d: invalid queue value", n+1)
			}
		}
		row := socket{Protocol: f[0], State: f[1], Local: f[4], Peer: f[5]}
		if len(f) > 6 {
			owner := strings.Join(f[6:], " ")
			if !strings.HasPrefix(owner, "users:((") {
				return nil, fmt.Errorf("line %d: unknown owner format", n+1)
			}
			row.Owner = owner
			row.OwnerKnown = true
		}
		rows = append(rows, row)
	}
	return rows, nil
}
func collectPorts(ctx context.Context, s *Section) {
	raw, ok := command(ctx, s, "ss", "-H", "-lntup")
	if !ok {
		return
	}
	rows, err := parseSockets(raw)
	if err != nil {
		issue(s, "parse_error", "ss", err.Error())
		return
	}
	enrichSockets(ctx, s, rows)
	s.Data = rows
	missing := 0
	for _, r := range rows {
		if !r.OwnerKnown {
			missing++
		}
	}
	if missing > 0 {
		issue(s, "incomplete_ownership", "ss", fmt.Sprintf("Owner not reported for %d sockets; reason unknown (possibly permission restrictions or a race)", missing))
	}
	s.Notes = []string{"An empty successful response means ss listed no matching sockets in this namespace at query time. UDP UNCONN does not confirm a working service. Docker ports published through NAT may be absent from ss; see containers. External reachability and firewall rules are not evaluated here.", "PID-to-executable/cgroup lookup follows ss; processes may exit and PIDs may be reused. Names ending in .service from cgroups are candidates; ownership is not confirmed through systemd."}
}

type service struct {
	Unit        string `json:"unit"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

func parseServices(raw string) ([]service, error) {
	rows := []service{}
	for n, l := range nonemptyLines(raw) {
		f := strings.Fields(l)
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
			return nil, fmt.Errorf("line %d: unknown unit format", n+1)
		}
		rows = append(rows, service{f[0], f[1], f[2], f[3], strings.Join(f[4:], " ")})
	}
	return rows, nil
}
func collectServices(ctx context.Context, s *Section) {
	d := map[string]any{}
	if raw, ok := command(ctx, s, "systemctl", "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager"); ok {
		if rows, err := parseServices(raw); err != nil {
			issue(s, "parse_error", "systemctl list-units", err.Error())
		} else {
			d["loaded_units"] = rows
		}
	}
	if raw, ok := command(ctx, s, "systemctl", "list-unit-files", "--type=service", "--no-legend", "--no-pager"); ok {
		rows := []map[string]string{}
		valid := true
		for _, l := range nonemptyLines(raw) {
			f := strings.Fields(l)
			if len(f) < 2 || len(f) > 3 || !strings.HasSuffix(f[0], ".service") {
				issue(s, "parse_error", "systemctl list-unit-files", "Unexpected format")
				valid = false
				break
			}
			row := map[string]string{"unit": f[0], "unit_file_state": f[1]}
			if len(f) == 3 {
				row["preset"] = f[2]
			}
			rows = append(rows, row)
		}
		if valid {
			d["unit_files"] = rows
		}
	}
	if len(d) > 0 {
		s.Data = d
	}
	s.Notes = []string{"active describes systemd state, not application availability. inactive and disabled do not by themselves indicate a fault."}
}
func collectTimers(ctx context.Context, s *Section) {
	raw, ok := command(ctx, s, "systemctl", "list-timers", "--all", "--no-pager")
	if !ok {
		return
	}
	// Preserve the human table rather than guessing locale/version-dependent columns.
	if !strings.Contains(raw, "NEXT") || !strings.Contains(raw, "ACTIVATES") {
		issue(s, "parse_error", "systemctl list-timers", "Expected table headers not found")
		return
	}
	s.Data = map[string]any{"table_reported": nonemptyLines(raw)}
	s.Notes = []string{"The table is preserved as reported by systemctl; dates are not parsed. Missed runs and job results are not inferred from this table."}
}

type cronFile struct {
	Path  string   `json:"path"`
	Kind  string   `json:"kind"`
	Lines []string `json:"active_lines,omitempty"`
	Mode  string   `json:"mode,omitempty"`
}

func collectCron(ctx context.Context, s *Section) {
	rows := []cronFile{}
	coverage := []map[string]string{}
	readCron := func(path, kind string) {
		if ctx.Err() != nil {
			return
		}
		if b, ok := file(s, path); ok {
			lines := []string{}
			for _, l := range strings.Split(string(b), "\n") {
				trim := strings.TrimSpace(l)
				if trim != "" && !strings.HasPrefix(trim, "#") {
					lines = append(lines, l)
				}
			}
			rows = append(rows, cronFile{Path: path, Kind: kind, Lines: lines})
		}
	}
	path := "/etc/crontab"
	if _, err := os.Stat(path); err == nil {
		readCron(path, "system_crontab")
	} else if os.IsNotExist(err) {
		coverage = append(coverage, map[string]string{"path": path, "result": "path_absent"})
	} else {
		issue(s, "error", path, err.Error())
	}
	dirs := []struct {
		path, kind string
		read       bool
	}{{"/etc/cron.d", "system_crontab", true}, {"/var/spool/cron/crontabs", "user_spool", true}, {"/var/spool/cron", "user_spool", true}, {"/etc/cron.hourly", "periodic_directory_entry", false}, {"/etc/cron.daily", "periodic_directory_entry", false}, {"/etc/cron.weekly", "periodic_directory_entry", false}, {"/etc/cron.monthly", "periodic_directory_entry", false}}
	for _, dir := range dirs {
		if ctx.Err() != nil {
			return
		}
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			if os.IsNotExist(err) {
				coverage = append(coverage, map[string]string{"path": dir.path, "result": "path_absent"})
			} else {
				kind := "error"
				if os.IsPermission(err) {
					kind = "denied"
				}
				issue(s, kind, dir.path, err.Error())
			}
			continue
		}
		coverage = append(coverage, map[string]string{"path": dir.path, "result": "directory_listed"})
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			p := filepath.Join(dir.path, entry.Name())
			info, err := entry.Info()
			if err != nil {
				issue(s, "error", p, err.Error())
				continue
			}
			if !info.Mode().IsRegular() {
				issue(s, "unsupported_entry", p, "Not a regular file; contents were not read")
				continue
			}
			if dir.read {
				readCron(p, dir.kind)
			} else {
				rows = append(rows, cronFile{Path: p, Kind: dir.kind, Mode: info.Mode().String()})
			}
		}
	}
	if b, ok := optionalFile(s, "/etc/anacrontab"); ok {
		lines := []string{}
		for _, l := range nonemptyLines(string(b)) {
			if !strings.HasPrefix(strings.TrimSpace(l), "#") {
				lines = append(lines, l)
			}
		}
		rows = append(rows, cronFile{Path: "/etc/anacrontab", Kind: "anacrontab", Lines: lines})
	}
	s.Data = map[string]any{"files": rows, "coverage": coverage}
	s.Notes = []string{"Lines are shown without interpretation; they may include environment assignments or invalid jobs. File presence does not prove cron reads or executes it. Periodic directory entries are candidates; run-parts rules are not checked. Custom spool paths, at jobs and user systemd timers are not checked."}
}
func optionalFile(s *Section, path string) ([]byte, bool) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, false
	}
	return file(s, path)
}

type container struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	Image string `json:"Image"`
	State struct {
		Status     string `json:"Status"`
		Running    *bool  `json:"Running"`
		ExitCode   *int   `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	RestartCount   *int                       `json:"RestartCount"`
	NetworkMode    string                     `json:"NetworkMode"`
	Networks       map[string]json.RawMessage `json:"Networks"`
	Ports          map[string]json.RawMessage `json:"Ports"`
	Mounts         []json.RawMessage          `json:"Mounts"`
	RestartPolicy  json.RawMessage            `json:"RestartPolicy"`
	ComposeProject string                     `json:"ComposeProject"`
	ComposeService string                     `json:"ComposeService"`
}

// Explicit projection: never request Config.Env, command arguments, health logs or labels en masse.
const inspectTemplate = `{"Id":{{json .Id}},"Name":{{json .Name}},"Image":{{json .Config.Image}},"State":{"Status":{{json .State.Status}},"Running":{{json .State.Running}},"ExitCode":{{json .State.ExitCode}},"StartedAt":{{json .State.StartedAt}},"FinishedAt":{{json .State.FinishedAt}},"Health":{{if .State.Health}}{"Status":{{json .State.Health.Status}}}{{else}}null{{end}}},"RestartCount":{{json .RestartCount}},"NetworkMode":{{json .HostConfig.NetworkMode}},"Networks":{{json .NetworkSettings.Networks}},"Ports":{{json .NetworkSettings.Ports}},"Mounts":{{json .Mounts}},"RestartPolicy":{{json .HostConfig.RestartPolicy}},"ComposeProject":{{json (index .Config.Labels "com.docker.compose.project")}},"ComposeService":{{json (index .Config.Labels "com.docker.compose.service")}}}`

func collectContainers(ctx context.Context, s *Section) {
	d := map[string]any{}
	s.Data = d
	if raw, ok := command(ctx, s, "docker", "context", "show"); ok {
		name := strings.TrimSpace(raw)
		if name == "" || strings.Contains(name, "\n") {
			issue(s, "parse_error", "docker context show", "Expected a single context name")
		} else {
			d["cli_context_name"] = name
		}
	}
	for _, name := range []string{"DOCKER_HOST", "DOCKER_CONTEXT"} {
		if os.Getenv(name) != "" {
			d[strings.ToLower(name)+"_override_present"] = true
		}
	}
	s.Notes = []string{"Docker CLI may use a remote daemon; association with this machine is unverified. Context/DOCKER_HOST are not overridden. Running does not mean healthy; absent Health means no healthcheck was reported. A stopped container exit code is not classified as a failure. Networks and ports describe Docker configuration, not connectivity tests. Env secrets and healthcheck logs are not requested."}
	raw, ok := command(ctx, s, "docker", "ps", "--all", "--quiet", "--no-trunc")
	if !ok {
		return
	}
	ids := nonemptyLines(raw)
	for _, id := range ids {
		if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
			issue(s, "parse_error", "docker ps", "Expected a full hexadecimal container ID")
			return
		}
	}
	d["listed_count"] = len(ids)
	rows := []container{}
	if len(ids) > 200 {
		issue(s, "truncated", "docker ps", "More than 200 containers: inspect is limited to the first 200")
		ids = ids[:200]
	}
	// Small batches preserve valid results if a container disappears during the snapshot.
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		raw, ok := command(ctx, s, "docker", "inspect", "--type", "container", "--format", inspectTemplate, id)
		if !ok {
			continue
		}
		c, err := parseContainer(raw, id)
		if err != nil {
			issue(s, "parse_error", "docker inspect "+id, err.Error())
			continue
		}
		rows = append(rows, c)
	}
	d["containers"] = rows
}
func parseContainer(raw, id string) (container, error) {
	var c container
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, err
	}
	if c.ID != id || c.Name == "" || c.State.Status == "" || c.State.Running == nil || c.State.ExitCode == nil || c.RestartCount == nil {
		return c, fmt.Errorf("missing fields or ID mismatch; record discarded")
	}
	return c, nil
}
func collectFirewall(ctx context.Context, s *Section) {
	raw, ok := command(ctx, s, "nft", "--json", "list", "ruleset")
	if !ok {
		return
	}
	var d map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		issue(s, "parse_error", "nft", err.Error())
		return
	}
	var rules []json.RawMessage
	if v, ok := d["nftables"]; !ok || json.Unmarshal(v, &rules) != nil || string(v) == "null" {
		issue(s, "parse_error", "nft", "Expected an nftables array")
		return
	}
	s.Data = d
	s.Notes = []string{"These are nftables rules; effective port reachability is not calculated. An empty nftables ruleset does not imply there are no other filters. Legacy iptables, eBPF, security groups and external firewalls were not checked."}
}
