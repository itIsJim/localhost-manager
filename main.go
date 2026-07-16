// localhost-manager — see what's running behind localhost:<port> and manage it.
//
// Zero third-party dependencies. Scans with `lsof` + `ps` (macOS/Linux),
// keeps only listeners reachable at localhost, classifies each as
// active / pending / stale, HTTP-sniffs each port to identify the app,
// and lets you kill the owning process.
//
//	active  — listening and currently in use (has established connections)
//	pending — listening but not used yet (no connections; may still be
//	          compiling / warming up)
//	stale   — process up longer than STALE_HOURS, or the port is stuck
//	          (shows LISTEN but a TCP probe can't connect)
package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed public/index.html
var publicFS embed.FS

var (
	listenPort   = envInt("PORT", 4321)
	staleHours   = envInt("STALE_HOURS", 24)
	probeTimeout = 900 * time.Millisecond
	selfPID      = os.Getpid()
)

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

type PortInfo struct {
	Port        int      `json:"port"`
	PID         int      `json:"pid"`
	Command     string   `json:"command"`
	Args        string   `json:"args"`
	User        string   `json:"user"`
	Addrs       []string `json:"addrs"`
	UptimeSec   *int64   `json:"uptimeSec"`
	CPU         float64  `json:"cpu"`
	Connections int      `json:"connections"`
	HTTPInfo    string   `json:"http"`
	Status      string   `json:"status"`
	Reasons     []string `json:"reasons"`
	Self        bool     `json:"self"`
	Killable    bool     `json:"killable"`
}

func run(cmd string, args ...string) string {
	// lsof exits non-zero when some fds can't be read; stdout is still usable.
	out, _ := exec.Command(cmd, args...).Output()
	return string(out)
}

type listener struct {
	pid          int
	command, usr string
	addr         string
	port         int
}

// parseLsofListen parses `lsof -F pcnuL` machine output.
func parseLsofListen(out string) []listener {
	var rows []listener
	var pid int
	var command, usr string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		tag, val := line[0], line[1:]
		switch tag {
		case 'p':
			pid, _ = strconv.Atoi(val)
		case 'c':
			command = val
		case 'L':
			usr = val
		case 'n':
			// e.g. "*:3000", "127.0.0.1:8080", "[::1]:5432"
			i := strings.LastIndex(val, ":")
			if i == -1 {
				continue
			}
			port, err := strconv.Atoi(val[i+1:])
			if err != nil {
				continue
			}
			rows = append(rows, listener{pid, command, usr, val[:i], port})
		}
	}
	return rows
}

// parseLsofEstablished counts established connections per local port.
func parseLsofEstablished(out string) map[int]int {
	counts := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		local, _, _ := strings.Cut(line[1:], "->")
		i := strings.LastIndex(local, ":")
		if i == -1 {
			continue
		}
		if port, err := strconv.Atoi(local[i+1:]); err == nil {
			counts[port]++
		}
	}
	return counts
}

// reachableAtLocalhost reports whether a bind address answers on localhost.
func reachableAtLocalhost(addr string) bool {
	a := strings.Trim(addr, "[]")
	return addr == "*" || a == "::" || a == "::1" || a == "localhost" ||
		strings.HasPrefix(a, "127.")
}

// parseEtime converts ps "[[dd-]hh:]mm:ss" to seconds.
func parseEtime(s string) *int64 {
	var days int64
	if d, rest, found := strings.Cut(s, "-"); found {
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil {
			return nil
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return nil
	}
	total := days * 86400
	multipliers := []int64{1, 60, 3600}
	for i := 0; i < len(parts); i++ {
		n, err := strconv.ParseInt(parts[len(parts)-1-i], 10, 64)
		if err != nil {
			return nil
		}
		total += n * multipliers[i]
	}
	return &total
}

type procInfo struct {
	uptimeSec *int64
	cpu       float64
	args      string
}

var psLine = regexp.MustCompile(`^\s*(\d+)\s+(\S+)\s+(\S+)\s+(.*)$`)

func getProcessInfo(pids []int) map[int]procInfo {
	info := map[int]procInfo{}
	if len(pids) == 0 {
		return info
	}
	strs := make([]string, len(pids))
	for i, p := range pids {
		strs[i] = strconv.Itoa(p)
	}
	out := run("ps", "-p", strings.Join(strs, ","), "-o", "pid=,etime=,%cpu=,args=")
	for _, line := range strings.Split(out, "\n") {
		m := psLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		cpu, _ := strconv.ParseFloat(m[3], 64)
		info[pid] = procInfo{parseEtime(m[2]), cpu, m[4]}
	}
	return info
}

// probeHost picks a dialable localhost address for a set of bind addresses:
// "[::1]" for IPv6-only listeners, "127.0.0.1" otherwise.
func probeHost(addrs []string) string {
	for _, addr := range addrs {
		a := strings.Trim(addr, "[]")
		if addr == "*" || a == "::" || a == "localhost" || strings.HasPrefix(a, "127.") {
			return "127.0.0.1"
		}
	}
	return "[::1]"
}

// probeTCP reports "open" or "stuck".
func probeTCP(host string, port int) string {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), probeTimeout)
	if err != nil {
		return "stuck"
	}
	conn.Close()
	return "open"
}

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>\s*(.*?)\s*</title>`)

// sniffHTTP identifies what's serving a port: "HTTP 200 · My App" or "".
func sniffHTTP(host string, port int) string {
	client := &http.Client{
		Timeout: 1200 * time.Millisecond,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/", host, port))
	if err != nil {
		return "" // not speaking HTTP (database, ssh, ...) — that's fine
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	s := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if m := titleRe.FindSubmatch(body); m != nil && len(m[1]) > 0 {
		title := strings.Join(strings.Fields(string(m[1])), " ")
		if len(title) > 80 {
			title = title[:80] + "…"
		}
		return s + " · " + title
	}
	if server := resp.Header.Get("Server"); server != "" {
		return s + " · " + server
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		return s + " → " + loc
	}
	return s
}

func classify(uptimeSec *int64, probeResult string, established int) (string, []string) {
	var reasons []string
	if uptimeSec != nil && *uptimeSec > int64(staleHours)*3600 {
		reasons = append(reasons, fmt.Sprintf("process up for over %dh", staleHours))
	}
	if probeResult == "stuck" {
		reasons = append(reasons, "port not accepting connections (stuck)")
	}
	if len(reasons) > 0 {
		return "stale", reasons
	}
	if established > 0 {
		return "active", []string{fmt.Sprintf("%d open connection(s)", established)}
	}
	return "pending", []string{"listening but no connections yet"}
}

func scanPorts() []PortInfo {
	var listenOut, estOut string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); listenOut = run("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pcnuL") }()
	go func() { defer wg.Done(); estOut = run("lsof", "-nP", "-iTCP", "-sTCP:ESTABLISHED", "-F", "pn") }()
	wg.Wait()

	established := parseLsofEstablished(estOut)

	// Only listeners you can actually reach at localhost:<port>;
	// dedupe to one row per (pid, port), merging bind addresses.
	type key struct{ pid, port int }
	byKey := map[key]*PortInfo{}
	var order []key
	for _, l := range parseLsofListen(listenOut) {
		if !reachableAtLocalhost(l.addr) {
			continue
		}
		k := key{l.pid, l.port}
		if row, ok := byKey[k]; ok {
			if !contains(row.Addrs, l.addr) {
				row.Addrs = append(row.Addrs, l.addr)
			}
			continue
		}
		byKey[k] = &PortInfo{
			Port: l.port, PID: l.pid, Command: l.command, User: l.usr,
			Addrs: []string{l.addr},
		}
		order = append(order, k)
	}

	pidSet := map[int]bool{}
	var pids []int
	for _, k := range order {
		if !pidSet[k.pid] {
			pidSet[k.pid] = true
			pids = append(pids, k.pid)
		}
	}
	procs := getProcessInfo(pids)

	currentUser := "?"
	if u, err := user.Current(); err == nil {
		currentUser = u.Username
	}

	// Probe + sniff all ports concurrently.
	var pwg sync.WaitGroup
	for _, k := range order {
		row := byKey[k]
		pwg.Add(1)
		go func(r *PortInfo) {
			defer pwg.Done()
			host := probeHost(r.Addrs)
			probeResult := probeTCP(host, r.Port)
			if probeResult == "open" {
				r.HTTPInfo = sniffHTTP(host, r.Port)
			}
			p := procs[r.PID]
			r.Args = p.args
			r.UptimeSec = p.uptimeSec
			r.CPU = p.cpu
			r.Connections = established[r.Port]
			r.Status, r.Reasons = classify(p.uptimeSec, probeResult, r.Connections)
			r.Self = r.PID == selfPID
			r.Killable = r.PID != selfPID && r.User == currentUser && r.PID > 1
		}(row)
	}
	pwg.Wait()

	rows := make([]PortInfo, 0, len(order))
	for _, k := range order {
		rows = append(rows, *byKey[k])
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Port != rows[j].Port {
			return rows[i].Port < rows[j].Port
		}
		return rows[i].PID < rows[j].PID
	})
	return rows
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Kill
// ---------------------------------------------------------------------------

type killResult struct {
	PID    int    `json:"pid"`
	Signal string `json:"signal"`
	Alive  bool   `json:"alive"`
}

func killPID(pid int, force bool) (*killResult, error) {
	var target *PortInfo
	for _, r := range scanPorts() {
		if r.PID == pid {
			target = &r
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("PID %d is not holding any localhost port", pid)
	}
	if !target.Killable {
		return nil, fmt.Errorf("PID %d (%s) is not killable from here", pid, target.Command)
	}
	sig, name := syscall.SIGTERM, "SIGTERM"
	if force {
		sig, name = syscall.SIGKILL, "SIGKILL"
	}
	if err := syscall.Kill(pid, sig); err != nil {
		return nil, err
	}
	// Report whether it actually died (give SIGTERM a moment).
	time.Sleep(700 * time.Millisecond)
	alive := syscall.Kill(pid, 0) == nil
	return &killResult{PID: pid, Signal: name, Alive: alive}, nil
}

// ---------------------------------------------------------------------------
// HTTP server
// ---------------------------------------------------------------------------

// isLocalName reports whether a Host/Origin hostname is a loopback name.
func isLocalName(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}

// guard blocks DNS-rebinding and cross-site requests: the Host header must
// be a loopback name, and non-GET requests must carry a JSON Content-Type
// (which forces a CORS preflight in browsers) and, if an Origin header is
// present, a loopback Origin.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLocalName(r.Host) {
			http.Error(w, "forbidden: non-local Host header", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !isLocalName(u.Host) {
					http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
					return
				}
			}
			if ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(ct) != "application/json" {
				http.Error(w, "expected Content-Type: application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		html, _ := publicFS.ReadFile("public/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(html)
	})

	mux.HandleFunc("GET /api/ports", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"staleHours": staleHours,
			"selfPort":   listenPort,
			"ports":      scanPorts(),
		})
	})

	mux.HandleFunc("POST /api/kill", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PID   int  `json:"pid"`
			Force bool `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PID <= 1 {
			writeJSON(w, 400, map[string]string{"error": "invalid pid"})
			return
		}
		res, err := killPID(req.PID, req.Force)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, res)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", listenPort)
	log.Printf("localhost-manager running at http://localhost:%d", listenPort)
	log.Fatal(http.ListenAndServe(addr, guard(mux)))
}
