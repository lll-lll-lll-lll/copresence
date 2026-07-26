package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"

	"github.com/lll-lll-lll-lll/copresence/internal/dashboard"
)

func cmdDashboard(args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	c := bind(fs, false)
	port := fs.Int("port", 8787, "port to listen on (0 picks a free one)")
	all := fs.Bool("all-projects", false, "start with spend unscoped from this workspace")
	open := fs.Bool("open", false, "open a browser once the server is up")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := findRoot(c.dir)
	if err != nil {
		return err
	}
	st, err := c.open()
	if err != nil {
		return err
	}
	defer st.Close()

	srv := dashboard.New(st, dashboard.Config{
		Session: c.session, Workspace: root, AllProjects: *all,
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Loopback only, and not configurable. The database holds the workspace
	// path and everything the agents said about the code; binding it to 0.0.0.0
	// would publish that to the network you happen to be on.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))
	return srv.Serve(ctx, addr, func(url string) {
		fmt.Printf("copresence dashboard for %q\n  %s\n  workspace: %s\n\nCtrl-C to stop.\n",
			c.session, url, trimPath(root))
		if *open {
			openBrowser(url)
		}
	})
}

// openBrowser is best-effort: failing to launch a browser is not a reason to
// tear down a server that is already listening and has printed its URL.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	if err := exec.Command(cmd, append(args, url)...).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "could not open a browser (%v); visit %s\n", err, url)
	}
}
