//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unsafe"
)

type displayOptions struct {
	color bool
	width int
}
type displayWriter struct {
	io.Writer
	options displayOptions
}

func outputOptions(w io.Writer, mode string) displayOptions {
	o := displayOptions{width: 100}
	terminal := false
	if f, ok := w.(*os.File); ok {
		var size struct{ rows, columns, x, y uint16 }
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
		if errno == 0 {
			terminal = true
			if size.columns > 0 {
				o.width = int(size.columns)
			}
		}
	}
	if o.width < 40 {
		o.width = 40
	}
	if o.width > 160 {
		o.width = 160
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	o.color = mode == "always" || (mode == "auto" && terminal && !noColor && os.Getenv("TERM") != "dumb")
	return o
}
func options(w io.Writer) displayOptions {
	if d, ok := w.(*displayWriter); ok {
		return d.options
	}
	return displayOptions{width: 100}
}
func paint(w io.Writer, code, text string) string {
	if options(w).color {
		return "\x1b[" + code + "m" + text + "\x1b[0m"
	}
	return text
}
func tone(status string) string {
	switch status {
	case "ok":
		return "36"
	case "partial", "warning", "timeout", "missing_tool", "unavailable", "truncated":
		return "33"
	default:
		return "31"
	}
}
func cellTone(value string) string {
	switch value {
	case "failed", "unhealthy", "error":
		return "31"
	case "healthy":
		return "32"
	case "active", "running", "LISTEN", "UP":
		return "36"
	case "not reported", "not established", "N/A", "starting", "activating", "restarting":
		return "33"
	}
	return ""
}
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}
func runeWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	if r >= 0x1100 && (r <= 0x115f || r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f || r >= 0xff01 && r <= 0xff60 || r >= 0x1f300 && r <= 0x1faff || r >= 0x20000 && r <= 0x3ffff) {
		return 2
	}
	return 1
}
func wrapText(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	text = clean(text)
	if text == "" {
		return []string{""}
	}
	lines := []string{}
	line := ""
	for _, word := range strings.Fields(text) {
		if line != "" && displayWidth(line)+1+displayWidth(word) <= width {
			line += " " + word
			continue
		}
		if line != "" {
			lines = append(lines, line)
			line = ""
		}
		chunk := ""
		for _, r := range word {
			if chunk != "" && displayWidth(chunk)+runeWidth(r) > width {
				lines = append(lines, chunk)
				chunk = ""
			}
			chunk += string(r)
		}
		line = chunk
	}
	if line != "" {
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}
func paragraph(w io.Writer, label, text string, dim bool) {
	width := options(w).width - 4
	prefix := label
	if prefix != "" {
		prefix += " "
	}
	lines := wrapText(prefix+text, width)
	for _, line := range lines {
		if dim {
			line = paint(w, "2", line)
		}
		fmt.Fprintln(w, "  "+line)
	}
}
func table(w io.Writer, header string, rows []string, full bool, name string) {
	headers := strings.Split(header, "\t")
	cols := len(headers)
	limit := 12
	if full || len(rows) < limit {
		limit = len(rows)
	}
	cells := make([][]string, limit)
	widths := make([]int, cols)
	for j, h := range headers {
		widths[j] = displayWidth(h)
	}
	for i, row := range rows[:limit] {
		parts := strings.Split(row, "\t")
		cells[i] = make([]string, cols)
		for j := 0; j < cols; j++ {
			v := "not reported"
			if j < len(parts) && parts[j] != "" {
				v = clean(parts[j])
			}
			cells[i][j] = v
			if n := displayWidth(v); n > widths[j] {
				widths[j] = n
			}
		}
	}
	available := options(w).width - 3*cols - 3
	if available < cols*6 {
		// Narrow terminals use labeled records instead of unreadably narrow columns.
		for i, row := range cells {
			fmt.Fprintln(w, "  "+paint(w, "2", fmt.Sprintf("── record %d ──", i+1)))
			for j, v := range row {
				paragraph(w, headers[j]+":", v, false)
			}
		}
	} else {
		for {
			total := 0
			largest := 0
			for j, n := range widths {
				total += n
				if n > widths[largest] {
					largest = j
				}
			}
			if total <= available {
				break
			}
			widths[largest]--
		}
		rule := func(left, mid, right string) {
			parts := make([]string, cols)
			for j, n := range widths {
				parts[j] = strings.Repeat("─", n+2)
			}
			fmt.Fprintln(w, "  "+paint(w, "2", left+strings.Join(parts, mid)+right))
		}
		writeRow := func(row []string, heading bool) {
			wrapped := make([][]string, cols)
			height := 1
			for j, v := range row {
				wrapped[j] = wrapText(v, widths[j])
				if len(wrapped[j]) > height {
					height = len(wrapped[j])
				}
			}
			for k := 0; k < height; k++ {
				fmt.Fprint(w, "  "+paint(w, "2", "│"))
				for j := range row {
					v := ""
					if k < len(wrapped[j]) {
						v = wrapped[j][k]
					}
					padding := strings.Repeat(" ", widths[j]-displayWidth(v))
					code := cellTone(row[j])
					if heading {
						code = "1;36"
					}
					if code != "" {
						v = paint(w, code, v)
					}
					fmt.Fprint(w, " "+v+padding+" "+paint(w, "2", "│"))
				}
				fmt.Fprintln(w)
			}
		}
		rule("┌", "┬", "┐")
		writeRow(headers, true)
		rule("├", "┼", "┤")
		for i, row := range cells {
			writeRow(row, false)
			multiline := false
			for j, v := range row {
				if displayWidth(v) > widths[j] {
					multiline = true
				}
			}
			if multiline && i < len(cells)-1 {
				rule("├", "┼", "┤")
			}
		}
		rule("└", "┴", "┘")
	}
	if len(rows) > limit {
		paragraph(w, "", fmt.Sprintf("… %d more records: srvpeek %s", len(rows)-limit, name), true)
	}
	if len(rows) == 0 {
		paragraph(w, "", "No records in the received response (see section scope and status).", true)
	}
}
func render(w io.Writer, r Report, full bool) {
	renderWithOptions(w, r, full, displayOptions{width: 100})
}
func renderWithOptions(out io.Writer, r Report, full bool, o displayOptions) {
	if o.width < 40 {
		o.width = 40
	}
	w := &displayWriter{out, o}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+paint(w, "1;36", "SRVPEEK")+paint(w, "2", "  v"+r.Version+"  ·  SYSTEM SNAPSHOT"))
	paragraph(w, "", fmt.Sprintf("%s  ·  %.2f s  ·  euid=%d", r.Started.Format("2006-01-02 15:04:05Z07:00"), float64(r.DurationMS)/1000, r.EUID), true)
	paragraph(w, "", "Collection status describes data availability, not system health. Unknown does not mean absent.", true)
	complete := 0
	for _, s := range r.Sections {
		if s.Status == "ok" {
			complete++
		}
	}
	fmt.Fprintln(w)
	paragraph(w, "COLLECTION", fmt.Sprintf("%d/%d sections collected without recorded issues", complete, len(r.Sections)), false)
	labels := map[string]string{"ok": "COLLECTED", "partial": "PARTIAL", "timeout": "TIMEOUT", "error": "ERROR", "denied": "ACCESS DENIED", "unavailable": "UNAVAILABLE", "missing_tool": "COMMAND NOT FOUND", "parse_error": "UNEXPECTED FORMAT", "truncated": "LIMIT EXCEEDED", "warning": "SOURCE WARNING"}
	// A compact overview makes unavailable sections visible before scrolling.
	for _, s := range r.Sections {
		label := labels[s.Status]
		if label == "" {
			label = clean(s.Status)
		}
		fmt.Fprintf(w, "  %-12s %s\n", strings.ToUpper(clean(s.Name)), paint(w, tone(s.Status), label))
	}
	for i, s := range r.Sections {
		label := labels[s.Status]
		if label == "" {
			label = clean(s.Status)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  "+paint(w, "2", strings.Repeat("━", o.width-4)))
		fmt.Fprintf(w, "  %s  %s  %s\n", paint(w, "1", fmt.Sprintf("%02d  %s", i+1, strings.ToUpper(clean(s.Name)))), paint(w, tone(s.Status), "["+label+"]"), paint(w, "2", strconv.FormatInt(s.DurationMS, 10)+" ms"))
		paragraph(w, "Scope:", s.Scope, true)
		fmt.Fprintln(w)
		if s.Data != nil {
			renderData(w, s, full)
		} else {
			paragraph(w, "", "Data unavailable. No empty inventory or health verdict is inferred.", false)
		}
		if len(s.Issues) > 0 {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  "+paint(w, "1;33", "COLLECTION ISSUES"))
			for _, e := range s.Issues {
				paragraph(w, "! "+clean(e.Kind)+":", e.Source, false)
				for _, l := range wrapText(e.Message, o.width-8) {
					fmt.Fprintln(w, "      "+paint(w, tone(e.Kind), l))
				}
			}
		}
		if len(s.Notes) > 0 {
			fmt.Fprintln(w)
			for _, n := range s.Notes {
				paragraph(w, "Note:", n, true)
			}
		}
	}
	fmt.Fprintln(w)
	paragraph(w, "", "Scope: current namespaces; sections are collected at different times. --json preserves structured data.", true)
	fmt.Fprintln(w)
}
func reported(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return "not reported"
	}
	return fmt.Sprint(v)
}
func durationText(seconds float64) string {
	n := int64(seconds)
	return fmt.Sprintf("%dd %02dh %02dm %02ds", n/86400, n/3600%24, n/60%60, n%60)
}
func portBindingsText(ports map[string]json.RawMessage) string {
	if len(ports) == 0 {
		return "none reported"
	}
	parts := []string{}
	for _, target := range sortedKeys(ports) {
		raw := ports[target]
		if string(raw) == "null" {
			parts = append(parts, target+" (no host binding reported)")
			continue
		}
		var bindings []struct {
			HostIP   string
			HostPort string
		}
		if err := json.Unmarshal(raw, &bindings); err != nil {
			parts = append(parts, target+": "+string(raw))
			continue
		}
		if len(bindings) == 0 {
			parts = append(parts, target+" (empty bindings reported)")
		}
		for _, b := range bindings {
			host := b.HostIP
			if host == "" {
				host = "[IP not reported]"
			} else if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			port := b.HostPort
			if port == "" {
				port = "[port not reported]"
			}
			parts = append(parts, host+":"+port+" → "+target)
		}
	}
	return strings.Join(parts, "; ")
}

// Preserve nested reported fields in detailed tables without guessing their meaning.
func fieldRows(value any, path string) []string {
	rows := []string{}
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			return []string{path + "\t{} (reported)"}
		}
		for _, k := range sortedKeys(v) {
			key := k
			if path != "" {
				key = path + "." + k
			}
			rows = append(rows, fieldRows(v[k], key)...)
		}
	case []any:
		if len(v) == 0 {
			return []string{path + "\t[] (reported)"}
		}
		for i, item := range v {
			rows = append(rows, fieldRows(item, fmt.Sprintf("%s[%d]", path, i))...)
		}
	case nil:
		rows = append(rows, path+"\tnull (reported)")
	default:
		rows = append(rows, path+"\t"+clean(fmt.Sprint(v)))
	}
	return rows
}
