package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"hop.vokh.dev/internal/client"
	"hop.vokh.dev/internal/proto"
)

// runPS prints what is currently served, across every device — the server is
// the only thing that knows, since each agent sees only its own tunnel.
func runPS(args []string) {
	fs := flag.NewFlagSet("hop ps", flag.ExitOnError)
	var (
		serverAddr = fs.String("server", envOr("HOP_SERVER", "hop.vokh.dev:7443"), "hop server control address")
		token      = fs.String("token", os.Getenv("HOP_TOKEN"), "agent token (or set HOP_TOKEN)")
		noTLS      = fs.Bool("no-tls", false, "connect without TLS (local development only)")
		asJSON     = fs.Bool("json", false, "print the listing as JSON")
		noColour   = fs.Bool("no-color", false, "disable colour")
	)
	fs.Usage = usage
	fs.Parse(args)

	if *token == "" {
		fmt.Fprintln(os.Stderr, "hop: no token; pass --token or set HOP_TOKEN")
		os.Exit(2)
	}

	tunnels, err := client.List(client.Config{
		Server: *serverAddr,
		Token:  *token,
		TLS:    !*noTLS,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hop: %v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		// Encoded from the wire types directly, so the JSON is the server's
		// answer rather than a re-rendering of the table.
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(tunnels); err != nil {
			fmt.Fprintf(os.Stderr, "hop: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Colour is decided the same way the request log decides it, and for the
	// same reason: a listing piped into grep should be plain text.
	initColourOn(os.Stdout, *noColour)

	// Which of these this machine is serving, and can therefore stop. An error
	// here is not worth failing the listing over — the table is still correct,
	// it just loses the PID column.
	local, _ := liveStates()
	printTable(tunnels, localPIDs(local))
}

// localPIDs maps subdomain to the PID serving it on this machine.
func localPIDs(local []State) map[string]int {
	out := make(map[string]int, len(local))
	for _, s := range local {
		if s.Subdomain != "" {
			out[s.Subdomain] = s.PID
		}
	}
	return out
}

func printTable(tunnels []proto.TunnelInfo, local map[string]int) {
	if len(tunnels) == 0 {
		fmt.Printf("\n  No tunnels are up.\n\n")
		return
	}

	// The owner column earns its place only when it distinguishes something.
	// With one token issued — the common case — every row would repeat the same
	// label, which is a column of noise. It appears by itself the moment a
	// second device connects, which is the only time the question "which
	// machine is serving that?" has an answer worth reading.
	owners := showOwners(tunnels)

	// The PID column appears only when something here is stoppable, by the same
	// rule: a column of blanks is worse than no column. It is what connects
	// this listing to `hop stop`, which reaches only this machine's processes.
	pids := false
	for _, t := range tunnels {
		if _, ok := local[t.Subdomain]; ok {
			pids = true
			break
		}
	}

	// Likewise the forwarded address: an agent from before the field existed
	// doesn't report one, and a table of dashes would be worse than no column.
	locals := false
	for _, t := range tunnels {
		if t.Local != "" {
			locals = true
			break
		}
	}

	// Widths come from the contents rather than being fixed: a subdomain may be
	// anything up to 32 characters, and a listing of short ones shouldn't leave
	// a gap sized for the longest possible name.
	wName, wOwner, wLocal := len("NAME"), len("OWNER"), len("FORWARDS TO")
	for _, t := range tunnels {
		wName = max(wName, len(t.Subdomain))
		wOwner = max(wOwner, len(t.Owner))
		wLocal = max(wLocal, len(t.Local))
	}

	fmt.Println()
	fmt.Printf("  %s  %s%s  %s%s%s\n",
		pad(paint("NAME", dim), "NAME", wName),
		column(owners, pad(paint("OWNER", dim), "OWNER", wOwner)),
		pad(paint("UP", dim), "UP", 7),
		column(pids, pad(paint("PID", dim), "PID", 7)),
		column(locals, pad(paint("FORWARDS TO", dim), "FORWARDS TO", wLocal)),
		paint("URL", dim),
	)
	for _, t := range tunnels {
		up := uptime(time.Duration(t.UptimeSeconds) * time.Second)
		// A dash, not a blank: "not this machine's" is a fact worth stating,
		// and an empty cell reads as missing data.
		here := "-"
		if pid, ok := local[t.Subdomain]; ok {
			here = strconv.Itoa(pid)
		}
		target := t.Local
		if target == "" {
			target = "-"
		}
		fmt.Printf("  %s  %s%s  %s%s%s\n",
			pad(paint(t.Subdomain, green), t.Subdomain, wName),
			column(owners, pad(paint(t.Owner, cyan), t.Owner, wOwner)),
			pad(paint(up, dim), up, 7),
			column(pids, pad(paint(here, dim), here, 7)),
			column(locals, pad(paint(target, dim), target, wLocal)),
			t.URL,
		)
	}

	if owners {
		fmt.Printf("\n  %d tunnel(s) up.\n\n", len(tunnels))
		return
	}
	// With the column hidden, name the one device in the footer instead, so the
	// information is still there for a listing that gets pasted somewhere.
	fmt.Printf("\n  %d tunnel(s) up, all on %s.\n\n", len(tunnels), paint(tunnels[0].Owner, cyan))
}

// showOwners reports whether more than one device is serving.
func showOwners(tunnels []proto.TunnelInfo) bool {
	if len(tunnels) == 0 {
		return false
	}
	for _, t := range tunnels[1:] {
		if t.Owner != tunnels[0].Owner {
			return true
		}
	}
	return false
}

// column renders a cell only when its column is being shown, trailing separator
// included, so the format string stays one line.
func column(show bool, cell string) string {
	if !show {
		return ""
	}
	return cell + "  "
}

// uptime renders a duration at the scale a tunnel actually lives on. Seconds
// stop being interesting once there are minutes of them, so each unit is shown
// with at most one below it.
func uptime(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
