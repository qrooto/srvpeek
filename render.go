//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func bytesText(v uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	f := float64(v)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
func pretty(w io.Writer, data any) {
	b, _ := json.MarshalIndent(data, "  ", "  ")
	for _, l := range strings.Split(string(b), "\n") {
		fmt.Fprintln(w, "  "+clean(l))
	}
}
func renderData(w io.Writer, s Section, full bool) {
	switch s.Name {
	case "system":
		d := s.Data.(map[string]any)
		rows := []string{}
		labels := []struct{ key, label string }{{"hostname", "Hostname"}, {"os", "OS"}, {"kernel", "Kernel"}, {"architecture", "Architecture"}, {"logical_cpus_visible", "Visible logical CPUs"}, {"uptime_seconds", "Uptime"}, {"load_1_5_15", "Load (1 / 5 / 15 min)"}}
		for _, item := range labels {
			if v, ok := d[item.key]; ok {
				value := fmt.Sprint(v)
				if item.key == "uptime_seconds" {
					if n, ok := v.(float64); ok {
						value = durationText(n)
					}
				}
				if item.key == "load_1_5_15" {
					if v, ok := v.([]float64); ok && len(v) == 3 {
						value = fmt.Sprintf("%.2f / %.2f / %.2f", v[0], v[1], v[2])
					}
				}
				rows = append(rows, item.label+"\t"+value)
			}
		}
		if m, ok := d["memory_bytes"].(map[string]uint64); ok {
			rows = append(rows, "RAM total / available\t"+bytesText(m["MemTotal"])+" / "+bytesText(m["MemAvailable"]), "Swap free / total\t"+bytesText(m["SwapFree"])+" / "+bytesText(m["SwapTotal"]))
		}
		table(w, "METRIC\tREPORTED VALUE", rows, true, s.Name)

	case "disks":
		d := s.Data.(map[string]any)
		rows := []string{}
		for _, v := range d["filesystems"].([]disk) {
			inode := "N/A"
			if v.InodesUsedPercent != nil {
				inode = fmt.Sprintf("%.1f%%", *v.InodesUsedPercent)
			}
			flag := ""
			used := "N/A"
			if v.UsedPercent != nil {
				used = fmt.Sprintf("%.1f%%", *v.UsedPercent)
			}
			if v.UsedPercent != nil && *v.UsedPercent >= 90 {
				flag = " [>=90% blocks used]"
			}
			if v.ReadOnly {
				flag += " [read-only]"
			}
			rows = append(rows, fmt.Sprintf("%s\t%s\t%s%s\t%s\t%s", v.Mount, v.FS, used, flag, bytesText(v.Available), inode))
		}
		table(w, "MOUNT\tFS\tUSED\tAVAILABLE\tINODES USED", rows, full, s.Name)
		if full {
			rows := []string{}
			counts := d["excluded_mounts_by_fs"].(map[string]int)
			for _, name := range sortedKeys(counts) {
				rows = append(rows, fmt.Sprintf("%s\t%d", name, counts[name]))
			}
			table(w, "EXCLUDED FS TYPE\tMOUNTS", rows, true, s.Name)
		}
	case "ports":
		rows := []string{}
		for _, p := range s.Data.([]socket) {
			owner := p.Owner
			if !p.OwnerKnown {
				owner = "not reported"
			}
			units := []string{}
			for _, proc := range p.Processes {
				units = append(units, proc.CgroupServiceCandidates...)
			}
			hint := strings.Join(units, ",")
			if hint == "" {
				hint = "not established"
			}
			rows = append(rows, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", p.Protocol, p.State, p.Local, owner, hint))
		}
		table(w, "PROTO\tSTATE\tLOCAL\tOWNER REPORTED BY SS\tCGROUP .service (candidate)", rows, full, s.Name)
		if full {
			rows := []string{}
			seen := map[int]bool{}
			for _, sock := range s.Data.([]socket) {
				for _, p := range sock.Processes {
					if seen[p.PID] {
						continue
					}
					seen[p.PID] = true
					rows = append(rows, fmt.Sprintf("%d\t%s\t%s", p.PID, p.Executable, strings.Join(p.CgroupServiceCandidates, ",")))
				}
			}
			table(w, "PID\tEXECUTABLE\tCGROUP .service (candidate)", rows, true, s.Name)
		}
	case "network":
		d := s.Data.(map[string]any)

		if ifs, ok := d["interfaces"].([]any); ok {
			rows := []string{}
			for _, entry := range ifs {
				m, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				addresses := []string{}
				if a, ok := m["addr_info"].([]any); ok {
					for _, v := range a {
						if a, ok := v.(map[string]any); ok {
							addresses = append(addresses, fmt.Sprintf("%v/%v", a["local"], a["prefixlen"]))
						}
					}
				}
				rows = append(rows, fmt.Sprintf("%s\t%s\t%s", reported(m, "ifname"), reported(m, "operstate"), strings.Join(addresses, ", ")))
			}
			table(w, "INTERFACE\tOPERSTATE\tADDRESSES", rows, full, s.Name)
		}
		for _, key := range []string{"routes_ipv4", "routes_ipv6"} {
			if routes, ok := d[key].([]any); ok {
				rows := []string{}
				for _, v := range routes {
					m, ok := v.(map[string]any)
					if !ok {
						continue
					}
					if !full && m["dst"] != "default" {
						continue
					}
					rows = append(rows, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", reported(m, "dst"), reported(m, "gateway"), reported(m, "dev"), reported(m, "table"), reported(m, "metric")))
				}
				fmt.Fprintf(w, "  %s: %d records received", key, len(routes))
				if !full {
					fmt.Fprint(w, "; showing default routes")
				}
				fmt.Fprintln(w)
				table(w, "DESTINATION\tGATEWAY\tDEVICE\tTABLE\tMETRIC", rows, full, s.Name)
			}
		}
		if v, ok := d["resolv_conf_directives"].([]string); ok {
			paragraph(w, "resolv.conf:", strings.Join(v, "; "), false)
		}
	case "services":
		d := s.Data.(map[string]any)
		if entries, ok := d["loaded_units"].([]service); ok {
			counts := map[string]int{}
			rows := []string{}
			others := []string{}
			for _, v := range entries {
				counts[v.Active]++
				row := fmt.Sprintf("%s\t%s\t%s\t%s", v.Unit, v.Load, v.Active, v.Sub)
				if v.Active == "failed" || v.Load != "loaded" {
					rows = append(rows, row)
				} else {
					others = append(others, row)
				}
			}
			totals := []string{}
			for _, k := range sortedKeys(counts) {
				totals = append(totals, fmt.Sprintf("%s=%d", k, counts[k]))
			}
			paragraph(w, "Loaded units:", strings.Join(totals, " · "), false)
			if full {
				rows = append(rows, others...)
			} else {
				fmt.Fprintln(w, "  Only failed or load != loaded units shown below; others: srvpeek services")
			}
			if len(rows) > 0 {
				table(w, "UNIT\tLOAD\tACTIVE\tSUB", rows, true, s.Name)
			} else {
				fmt.Fprintln(w, "  No units with failed or load != loaded in the received list.")
			}
		}
		if entries, ok := d["unit_files"].([]map[string]string); ok {
			counts := map[string]int{}
			for _, v := range entries {
				counts[v["unit_file_state"]]++
			}
			text := []string{}
			for _, k := range sortedKeys(counts) {
				text = append(text, fmt.Sprintf("%s=%d", k, counts[k]))
			}
			paragraph(w, "Unit files:", strings.Join(text, " · "), false)
			if full {
				rows := []string{}
				for _, entry := range entries {
					rows = append(rows, entry["unit"]+"\t"+entry["unit_file_state"]+"\t"+entry["preset"])
				}
				table(w, "UNIT FILE\tSTATE\tPRESET", rows, true, s.Name)
			}
		}
	case "timers":
		d := s.Data.(map[string]any)
		if lines, ok := d["table_reported"].([]string); ok {
			for i, l := range lines {
				if !full && i >= 14 {
					fmt.Fprintln(w, "  … full table: srvpeek timers")
					break
				}
				fmt.Fprintln(w, "  "+clean(l))
			}
		}
	case "cron":
		d := s.Data.(map[string]any)
		rows := []string{}
		for _, f := range d["files"].([]cronFile) {
			desc := fmt.Sprintf("%d non-comment lines (not necessarily jobs)", len(f.Lines))
			if f.Kind == "periodic_directory_entry" {
				desc = "run-parts candidate · " + f.Mode
			}
			rows = append(rows, fmt.Sprintf("%s\t%s", f.Path, desc))
		}
		table(w, "FILE\tCONTENTS", rows, full, s.Name)
		if full {
			for _, f := range d["files"].([]cronFile) {
				if len(f.Lines) == 0 {
					continue
				}
				paragraph(w, "File:", f.Path, false)
				for _, line := range f.Lines {
					paragraph(w, "|", line, false)
				}
			}
			coverage := []string{}
			for _, entry := range d["coverage"].([]map[string]string) {
				coverage = append(coverage, entry["path"]+"\t"+entry["result"])
			}
			table(w, "CHECKED PATH\tRESULT", coverage, true, s.Name)
		}
	case "containers":
		d := s.Data.(map[string]any)
		if v, ok := d["cli_context_name"]; ok {
			fmt.Fprintln(w, "  CLI context:", clean(fmt.Sprint(v)))
		}
		for _, k := range []string{"docker_host_override_present", "docker_context_override_present"} {
			if d[k] == true {
				fmt.Fprintln(w, "  "+k+": true")
			}
		}
		if n, ok := d["listed_count"]; ok {
			fmt.Fprintln(w, "  Listed by docker ps -a:", n)
		}
		if entries, ok := d["containers"].([]container); ok {
			rows := []string{}
			for _, c := range entries {
				health := "not reported"
				if c.State.Health != nil {
					health = c.State.Health.Status
				}
				ports := portBindingsText(c.Ports)
				rows = append(rows, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", strings.TrimPrefix(c.Name, "/"), c.State.Status, health, strings.Join(sortedKeys(c.Networks), ","), ports))
			}
			table(w, "NAME\tSTATE\tHEALTH\tNETWORKS\tDOCKER PORTS", rows, full, s.Name)
			if full {
				for _, c := range entries {
					paragraph(w, "Container:", c.Name, false)
					b, _ := json.Marshal(c)
					var reported any
					json.Unmarshal(b, &reported)
					table(w, "REPORTED FIELD\tVALUE", fieldRows(reported, ""), true, s.Name)
				}
			}
		}
	case "firewall":
		if full {
			fmt.Fprintln(w, "  Reported details:")
			pretty(w, s.Data)
		} else {
			fmt.Fprintln(w, "  JSON response received from nft; rule structure and effective port reachability were not analyzed.")
			fmt.Fprintln(w, "  Rules: srvpeek firewall")
		}
	default:
		fmt.Fprintln(w, "  Reported details:")
		pretty(w, s.Data)
	}
}
