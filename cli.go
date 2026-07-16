// cli.go — subcommand dispatch and the non-interactive terminal commands.
//
//	localhost-manager list          print the port table once
//	localhost-manager kill <port>   SIGTERM whatever holds <port> (--force → SIGKILL)
//	localhost-manager tui           full-screen interactive UI (see tui.go)

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/mattn/go-runewidth"
)

const usageText = `localhost-manager — see what's running at localhost:<port> and manage it

  localhost-manager              start the web UI at http://localhost:4321
  localhost-manager tui          interactive terminal UI
  localhost-manager list         print the port table once
  localhost-manager kill <port>  SIGTERM whatever is listening on <port>
                                 (add --force for SIGKILL)
`

func cliMain(args []string) {
	switch args[0] {
	case "tui":
		runTUI()
	case "list":
		cmdList()
	case "kill":
		cmdKill(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usageText)
		os.Exit(2)
	}
}

// ---------------------------------------------------------------------------
// Shared table cells (display-width aware, so CJK titles align)
// ---------------------------------------------------------------------------

func fmtUptime(sec *int64) string {
	if sec == nil {
		return "—"
	}
	s := *sec
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 86400:
		return fmt.Sprintf("%dh %dm", s/3600, (s%3600)/60)
	}
	return fmt.Sprintf("%dd %dh", s/86400, (s%86400)/3600)
}

// cell truncates/pads s to exactly w display columns, keeping one gap column.
func cell(s string, w int) string {
	if w < 2 {
		return strings.Repeat(" ", w)
	}
	s = runewidth.Truncate(s, w-1, "…")
	return runewidth.FillRight(s, w)
}

func whatColumn(r PortInfo) string {
	what := r.Command
	if r.HTTPInfo != "" {
		what += " · " + r.HTTPInfo
	}
	if r.Args != "" {
		what += " — " + r.Args
	}
	return what
}

// port(16) what(flex) pid(7) uptime(9) conns(6) cpu(6) status
const fixedCols = 16 + 7 + 9 + 6 + 6

func tableCells(port, what, pid, up, conns, cpu string, width int) string {
	flex := width - fixedCols - 12 // leave room for the status column
	if flex < 12 {
		flex = 12
	}
	return cell(port, 16) + cell(what, flex) +
		cell(pid, 7) + cell(up, 9) + cell(conns, 6) + cell(cpu, 6)
}

func rowCells(r PortInfo, width int) string {
	return tableCells("localhost:"+strconv.Itoa(r.Port), whatColumn(r),
		strconv.Itoa(r.PID), fmtUptime(r.UptimeSec),
		strconv.Itoa(r.Connections), fmt.Sprintf("%.1f", r.CPU), width)
}

func headerCells(width int) string {
	return tableCells("LOCALHOST", "WHAT'S RUNNING", "PID", "UPTIME", "CONNS", "CPU%", width) + "STATUS"
}

// ---------------------------------------------------------------------------
// list / kill
// ---------------------------------------------------------------------------

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
)

func ansiStatus(status string) string {
	switch status {
	case "active":
		return ansiGreen
	case "pending":
		return ansiYellow
	}
	return ansiRed
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func termWidth() int {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, _ := cmd.Output()
	var rows, cols int
	if _, err := fmt.Sscanf(string(out), "%d %d", &rows, &cols); err != nil || cols < 60 {
		return 120
	}
	return cols
}

func cmdList() {
	color := isTTY(os.Stdout)
	width := 120
	if isTTY(os.Stdin) {
		width = termWidth()
	}
	if color {
		fmt.Println(ansiDim + headerCells(width) + ansiReset)
	} else {
		fmt.Println(headerCells(width))
	}
	for _, r := range scanPorts() {
		status := r.Status
		if r.Self {
			status += " (this app)"
		}
		if color {
			fmt.Println(rowCells(r, width) + ansiStatus(r.Status) + status + ansiReset)
		} else {
			fmt.Println(rowCells(r, width) + status)
		}
	}
}

func cmdKill(args []string) {
	port, force := 0, false
	for _, a := range args {
		if a == "--force" || a == "-9" {
			force = true
			continue
		}
		p, err := strconv.Atoi(a)
		if err != nil || p <= 0 || p > 65535 {
			fmt.Fprintf(os.Stderr, "invalid port %q\n\n%s", a, usageText)
			os.Exit(2)
		}
		port = p
	}
	if port == 0 {
		fmt.Fprintf(os.Stderr, "kill: missing port\n\n%s", usageText)
		os.Exit(2)
	}
	var targets []PortInfo
	for _, r := range scanPorts() {
		if r.Port == port {
			targets = append(targets, r)
		}
	}
	if len(targets) == 0 {
		fmt.Fprintf(os.Stderr, "nothing is listening on localhost:%d\n", port)
		os.Exit(1)
	}
	failed := false
	for _, t := range targets {
		res, err := killPID(t.PID, force)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "%s (PID %d): %v\n", t.Command, t.PID, err)
			failed = true
		case res.Alive:
			fmt.Printf("%s (PID %d): sent %s, still alive (try --force)\n", t.Command, t.PID, res.Signal)
			failed = true
		default:
			fmt.Printf("%s (PID %d): exited after %s\n", t.Command, t.PID, res.Signal)
		}
	}
	if failed {
		os.Exit(1)
	}
}
