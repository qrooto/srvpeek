# srvpeek

[English](README.md) | [Русский](README.ru.md)

A single binary for inspecting an unfamiliar Linux server. Reports collection status, resources, network configuration, sockets and their owners, systemd services, scheduled tasks and Docker containers. Reads system state without modifying it and requires no additional runtime libraries.

## Install as `srvpeek`

Build with `make build`, then install the native binary into a directory on `PATH`:

```bash
sudo install -m 0755 dist/srvpeek /usr/local/bin/srvpeek
command -v srvpeek
srvpeek
```

For another server, choose the binary matching **that server's** architecture (`uname -m`):

```bash
# Run on the development machine; use srvpeek-linux-arm64 for aarch64 servers.
scp dist/srvpeek-linux-amd64 user@server:/tmp/srvpeek
```

Then, on the server:

```bash
sudo install -m 0755 /tmp/srvpeek /usr/local/bin/srvpeek
srvpeek
sudo srvpeek # optional: more visibility, subject to system restrictions
```

`/usr/local/bin` must be on `PATH`. Without administrator access, install to `~/.local/bin` and ensure that directory is on your shell's `PATH`. No Go installation is needed to run a prebuilt binary. Running `srvpeek` does not automatically elevate privileges; collection restrictions remain explicit in the report.

## Usage

Choose the binary for your server architecture (`uname -m`): `x86_64` → amd64, `aarch64` → arm64. `dist/srvpeek` is built for the development machine.

```bash
./dist/srvpeek
sudo ./dist/srvpeek                 # more process, cron and firewall visibility
./dist/srvpeek ports                # complete section
./dist/srvpeek containers
./dist/srvpeek --full
./dist/srvpeek --color always       # force ANSI colors, including through a pipe
./dist/srvpeek --color never        # plain text
./dist/srvpeek --timeout 8s
./dist/srvpeek --json
./dist/srvpeek --save snapshot.json # new file, permissions 0600; refuses to overwrite
```

Place flags before the section name. Sections: `system`, `disks`, `network`, `ports`, `services`, `timers`, `cron`, `containers`, `firewall`.

Collectors run concurrently. The default timeout is 4 seconds per section, including all sequential commands in that section. Each command has a 2 MiB limit for stdout and stderr; each file has a 2 MiB limit. Responses exceeding a limit are discarded. Timed-out commands and their process groups are terminated; blocked file reads do not hold up the report beyond the section timeout.

All interface text, help and diagnostics are in English. Documentation is available in English and Russian. Child commands run with `LC_ALL=C` and `LANG=C`. Names, paths, descriptions, cron contents and other source data are preserved as reported; their language depends on the inspected system.

## Terminal output

The text report starts with collection status for every section. Sections have distinct separators, bordered tables and wrapped notes and diagnostics. Tables adapt to terminal width; narrow terminals use labeled records. Docker host port bindings are displayed as `host:port → container-port/protocol`. Detailed text sections retain tabular presentation; use `--json` for the complete structured snapshot.

`--color auto` is the default: colors are enabled only when stdout is a terminal, `TERM` is not `dumb`, and `NO_COLOR` is unset. `--color always` forces colors; `--color never` disables them. Redirected output is plain text by default. JSON output and saved JSON snapshots never contain formatting escape sequences. Color describes reported states and collection issues, not an overall health verdict; labels remain visible without color.

## Status semantics

`ok` means data was collected within the stated scope. **It is not a system health verdict or confirmation that an application works.**

`partial` means some data was obtained but collection issues or source warnings occurred. Other statuses: `timeout`, `missing_tool`, `denied`, `unavailable`, `error`, `parse_error`, `truncated`, `warning`. Issues identify their source and are preserved in JSON. A failed command's stdout is discarded even if it looks valid. Even with exit code 0, any stderr causes a `warning` and stdout is not interpreted: some tools report access failures only through stderr. Unexpected formats are not interpreted as empty inventories.

JSON includes a schema version, report and section start times and durations, collection scopes, data, notes and issues. `data` is omitted when no useful data was obtained. A successful `[]` is an explicitly empty response from the queried source. Sections are not an atomic snapshot: processes and containers may change between queries.

Exit codes:

- `0`: all selected sections were collected without recorded issues; not a system health code.
- `1`: at least one section is incomplete or unavailable; the report is still produced.
- `2`: invalid arguments or a report write error.

## Coverage and limitations

| Section | Collected data | Limits on interpretation |
|---|---|---|
| system | OS, kernel, uptime, load, CPUs visible to the process, RAM/swap from procfs | Load is not CPU utilization. Procfs may describe the host rather than cgroup limits |
| disks | Selected local filesystem types, blocks, space available to unprivileged users, inodes, read-only status | Not an audit of all filesystems. Do not sum bind/overlay capacities. Network and pseudo-filesystems are excluded |
| network | `ip -j`: interfaces and IPv4/IPv6 routing tables; resolv.conf | No connectivity, DNS or internet tests; split-DNS rules may be missing |
| ports | `ss`: TCP LISTEN, UDP UNCONN, reported owners, executables and .service candidates from cgroups | No external reachability checks; NAT publications may be absent. Missing owners do not imply missing processes. PID matching is subject to races |
| services | Loaded system service units and installed unit files | `active` is not an application check; `inactive` and `disabled` are not diagnoses. User managers are excluded |
| timers | System systemd timer table without guessing date formats | No last-run result inference; user timers excluded |
| cron | `/etc/crontab`, `/etc/cron.d`, two standard spool paths, periodic directories, anacrontab | File presence does not prove loading or execution; lines may be assignments or invalid jobs. Custom spool paths and at jobs are excluded |
| containers | Docker CLI context, all containers, state/health, networks/IPs, ports, mounts, restart policy, Compose project/service | The context may refer to another machine. `running` does not imply `healthy`. Missing Health means no healthcheck was reported. Podman/CRI excluded |
| firewall | nftables JSON ruleset | No effective permission calculation. Legacy iptables, eBPF, external firewalls and cloud ACLs excluded |

System tools are used when available: `ip`, `ss`, `systemctl`, `docker`, `nft`. Missing commands are explicitly reported. Docker is not required to run the utility itself. Docker CLI uses the current context and environment; the report shows the context name and whether override variables are present, without exposing their values.

Terminal control characters are removed from displayed data. Docker Env, command arguments and healthcheck logs are not requested. Snapshots may still contain sensitive addresses, paths, mounts, names and cron contents. The utility does not upload reports; Docker CLI may contact a daemon according to the user's selected context.

## Build and test

Go 1.23 or newer; no external Go dependencies:

```bash
go test ./...
go vet ./...
make build
make release # static Linux amd64 and arm64 binaries
```

Or build directly:

```bash
CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags='-s -w' -o srvpeek .
```

This initial version does not perform HTTP/TCP probes, analyze logs, support Podman or compare snapshots. Saved JSON is available for analysis and future comparison.
