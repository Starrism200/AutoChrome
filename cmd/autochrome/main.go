// Command autochrome creates self-contained browser environments, launches
// Chrome on a virtual display, runs JavaScript scripts and serves an MCP
// server for AI clients.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"

	"autochrome/internal/browser"
	"autochrome/internal/desktop"
	"autochrome/internal/envdir"
	"autochrome/internal/fingerprint"
	"autochrome/internal/jsruntime"
	"autochrome/internal/mcpserver"
	"autochrome/internal/scripts"
	"autochrome/internal/session"
	"autochrome/internal/users"
)

const screen = "1366x900x24"

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "gen":
		cmdGen(os.Args[2:])
	case "fingerprint":
		cmdFingerprint(os.Args[2:])
	case "open":
		cmdOpen(os.Args[2:])
	case "user":
		cmdUser(os.Args[2:])
	case "script":
		cmdScript(os.Args[2:])
	case "mcp":
		cmdMCP(os.Args[2:])
	case "desktop":
		cmdDesktop(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `autochrome - browser + virtual display toolkit

Usage:
  autochrome gen         -id <id> [-user USER] [-platform NAME] [-root DIR]
  autochrome fingerprint -id <id>
  autochrome open        -id <id> [-user USER] [-url URL] [-proxy ADDR] [options]
  autochrome user        add <id> | list | show [id]
  autochrome script      list | run <name> [-input JSON]
  autochrome mcp         [-user USER] [--vnc] [--allow-eval] [--allow-scripts]
  autochrome desktop     [-user USER] [--vnc-password PASS] [--port-start N] [--port-end N]

The default user keeps the historical layout (profiles/, scripts/custom/, data/).
Other users live under data/users/<id>/.

Run "autochrome <command> -h" for command flags.
`)
	os.Exit(2)
}

func cmdGen(args []string) {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	userID := fs.String("user", "", "user id (default: $AUTOCHROME_USER or default)")
	root := fs.String("root", "", "environment root (default: the user's profiles dir)")
	id := fs.String("id", "", "environment id (required)")
	platform := fs.String("platform", "default", "platform name used in the directory name")
	_ = fs.Parse(args)

	if *id == "" {
		log.Fatal("gen: -id is required")
	}
	u, err := users.Resolve(*userID)
	if err != nil {
		log.Fatal(err)
	}
	envRoot := *root
	if envRoot == "" {
		envRoot = u.EnvRoot
	}
	fp := fingerprint.Generate(*id)
	dir, err := envdir.Create(envRoot, *platform, *id, fp)
	if err != nil {
		log.Fatalf("create environment: %v", err)
	}
	fmt.Printf("user=%s\n", u.ID)
	fmt.Printf("env_dir=%s\n", dir)
	fmt.Printf("fingerprint=%s\n", fingerprint.Encode(fp))
}

func cmdFingerprint(args []string) {
	fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
	id := fs.String("id", "", "environment id (required)")
	_ = fs.Parse(args)

	if *id == "" {
		log.Fatal("fingerprint: -id is required")
	}
	fmt.Println(fingerprint.Encode(fingerprint.Generate(*id)))
}

func cmdOpen(args []string) {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	userID := fs.String("user", "", "user id (default: $AUTOCHROME_USER or default)")
	root := fs.String("root", "", "environment root (default: the user's profiles dir)")
	id := fs.String("id", "", "environment id (required)")
	platform := fs.String("platform", "default", "platform name used in the directory name")
	chrome := fs.String("chrome", "/usr/bin/google-chrome", "Chrome executable")
	proxy := fs.String("proxy", "", "proxy address, e.g. socks5://127.0.0.1:10801")
	url := fs.String("url", "https://example.com", "page to open")
	headless := fs.Bool("headless", false, "run Chrome headless instead of on the virtual display")
	timeout := fs.Duration("timeout", 180*time.Second, "browser timeout")
	keep := fs.Bool("keep", false, "keep the browser open until Ctrl+C")
	screenshot := fs.String("screenshot", "", "optional screenshot path")
	slotDir := fs.String("slot-dir", "", "browser concurrency slot directory")
	slotMax := fs.Int("slot-max", 0, "max concurrent browsers (0 uses the user cap)")
	xvfb := fs.Bool("xvfb", true, "wrap in xvfb-run when DISPLAY is not set")
	vnc := fs.Bool("vnc", false, "start x11vnc + noVNC for this browser")
	vncPassword := fs.String("vnc-password", "", "optional VNC password (also $AUTOCHROME_VNC_PASSWORD)")
	vncStart := fs.Int("vnc-port-start", 0, "override first port for auto-assigned VNC/noVNC ports")
	vncEnd := fs.Int("vnc-port-end", 0, "override last port for auto-assigned VNC/noVNC ports")
	_ = fs.Parse(args)

	if *id == "" {
		log.Fatal("open: -id is required")
	}
	if !*vnc && *xvfb {
		runUnderXvfb()
	}
	u, err := users.Resolve(*userID)
	if err != nil {
		log.Fatal(err)
	}
	var displayEnv []string
	if *vnc {
		d, err := startDesktop(u, *vncPassword, *vncStart, *vncEnd)
		if err != nil {
			log.Fatalf("vnc: %v", err)
		}
		defer d.Close()
		displayEnv = d.Display().Env()
		log.Printf("vnc: %s (display %s)", d.Info().URL, d.Info().Display)
	}

	envRoot := *root
	if envRoot == "" {
		envRoot = u.EnvRoot
	}
	proxyAddr := *proxy
	if proxyAddr == "" {
		proxyAddr = u.DefaultProxy
	}
	slot := *slotDir
	if slot == "" {
		slot = u.SlotDir
	}
	max := *slotMax
	if max == 0 {
		max = u.BrowserMax()
	}

	fp := fingerprint.Generate(*id)
	dir := filepath.Join(envRoot, envdir.Name(*platform, *id))
	if meta, ok := envdir.ReadMeta(dir); ok && meta.Fingerprint.UserAgent != "" {
		fp = meta.Fingerprint
	} else if _, err := envdir.Create(envRoot, *platform, *id, fp); err != nil {
		log.Fatalf("create environment: %v", err)
	}

	ctx, cleanup, err := browser.Launch(browser.Options{
		ChromePath:  *chrome,
		ProfileDir:  dir,
		ProxyAddr:   proxyAddr,
		Headless:    *headless,
		Timeout:     *timeout,
		Fingerprint: &fp,
		SlotDir:     slot,
		SlotMax:     max,
		Env:         displayEnv,
	})
	if err != nil {
		log.Fatalf("launch browser: %v", err)
	}
	defer cleanup()

	log.Printf("user=%s env=%s proxy=%q headless=%v", u.ID, dir, proxyAddr, *headless)
	if err := chromedp.Do(ctx, chromedp.Navigate(*url)); err != nil {
		log.Fatalf("navigate: %v", err)
	}
	if err := chromedp.Do(ctx, chromedp.Sleep(3*time.Second)); err != nil {
		log.Fatalf("wait: %v", err)
	}
	location, _ := chromedp.Run(ctx, chromedp.Location())
	title, _ := chromedp.Run(ctx, chromedp.Title())
	log.Printf("url=%s title=%q", location, title)

	if *screenshot != "" {
		path := *screenshot
		if !filepath.IsAbs(path) {
			path = filepath.Join(u.ShotRoot, path)
		}
		if err := browser.Screenshot(ctx, path); err != nil {
			log.Printf("screenshot: %v", err)
		} else {
			log.Printf("screenshot: %s", path)
		}
	}

	if *keep {
		log.Printf("browser kept open; press Ctrl+C to close")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
	}
}

func cmdUser(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: autochrome user add <id> | list | show [id]")
		os.Exit(2)
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("user add", flag.ExitOnError)
		allowEval := fs.Bool("allow-eval", false, "allow browser_eval for this user")
		allowScripts := fs.Bool("allow-scripts", false, "allow script_run for this user")
		maxBrowsers := fs.Int("max-browsers", 0, "maximum concurrent browsers")
		maxEnvs := fs.Int("max-envs", 0, "maximum environments (advisory)")
		proxy := fs.String("proxy", "", "default proxy for this user")
		vncStart := fs.Int("vnc-port-start", 0, "first port for auto-assigned VNC/noVNC ports")
		vncEnd := fs.Int("vnc-port-end", 0, "last port for auto-assigned VNC/noVNC ports")
		id, rest := splitPositional(args[1:])
		_ = fs.Parse(rest)
		if id == "" {
			id = fs.Arg(0)
		}
		if id == "" {
			log.Fatal("user add: id is required")
		}
		u, err := users.Add(id)
		if err != nil {
			log.Fatalf("add user: %v", err)
		}
		if *allowEval {
			u.AllowEval = true
		}
		if *allowScripts {
			u.AllowScripts = true
		}
		if *maxBrowsers > 0 {
			u.MaxBrowsers = *maxBrowsers
		}
		if *maxEnvs > 0 {
			u.MaxEnvs = *maxEnvs
		}
		if *proxy != "" {
			u.DefaultProxy = *proxy
		}
		if *vncStart > 0 {
			u.VNCPortStart = *vncStart
		}
		if *vncEnd > 0 {
			u.VNCPortEnd = *vncEnd
		}
		if err := u.Save(); err != nil {
			log.Fatalf("save user: %v", err)
		}
		printJSON(u)

	case "list":
		list, err := users.List()
		if err != nil {
			log.Fatalf("list users: %v", err)
		}
		printJSON(list)

	case "show":
		fs := flag.NewFlagSet("user show", flag.ExitOnError)
		id, rest := splitPositional(args[1:])
		_ = fs.Parse(rest)
		if id == "" {
			id = fs.Arg(0)
		}
		u, err := users.Resolve(id)
		if err != nil {
			log.Fatal(err)
		}
		printJSON(u)

	default:
		fmt.Fprintf(os.Stderr, "unknown user command %q\n", args[0])
		os.Exit(2)
	}
}

func cmdScript(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: autochrome script list | run <name> [-input JSON]")
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("script list", flag.ExitOnError)
		userID := fs.String("user", "", "user id")
		_ = fs.Parse(args[1:])
		u, err := users.Resolve(*userID)
		if err != nil {
			log.Fatal(err)
		}
		list, err := scripts.List(u.ScriptRoot)
		if err != nil {
			log.Fatalf("list scripts: %v", err)
		}
		printJSON(list)

	case "run":
		fs := flag.NewFlagSet("script run", flag.ExitOnError)
		userID := fs.String("user", "", "user id")
		chrome := fs.String("chrome", "/usr/bin/google-chrome", "Chrome executable")
		inputJSON := fs.String("input", "", "JSON input object")
		inputFile := fs.String("input-file", "", "path to a JSON input file")
		sessionID := fs.String("session", "", "existing session id exposed as input.session_id")
		name, rest := splitPositional(args[1:])
		_ = fs.Parse(rest)
		if name == "" {
			name = fs.Arg(0)
		}
		if name == "" {
			log.Fatal("script run: name is required")
		}
		u, err := users.Resolve(*userID)
		if err != nil {
			log.Fatal(err)
		}
		if !u.AllowScripts {
			log.Fatalf("scripts are disabled for user %q", u.ID)
		}
		script, src, err := scripts.Load(u.ScriptRoot, name)
		if err != nil {
			log.Fatal(err)
		}
		input := map[string]any{}
		if *inputFile != "" {
			b, err := os.ReadFile(*inputFile)
			if err != nil {
				log.Fatalf("read input: %v", err)
			}
			if err := json.Unmarshal(b, &input); err != nil {
				log.Fatalf("parse input: %v", err)
			}
		}
		if *inputJSON != "" {
			if err := json.Unmarshal([]byte(*inputJSON), &input); err != nil {
				log.Fatalf("parse -input: %v", err)
			}
		}
		if *sessionID != "" {
			input["session_id"] = *sessionID
		}
		mgr := session.NewManager(u, *chrome)
		defer mgr.CloseAll()
		runner := jsruntime.New(u, mgr, os.Stderr)
		runner.AllowEval = u.AllowEval
		runner.AutoClose = true
		runner.Timeout = script.TimeoutDuration()
		result, err := runner.Run(src, input)
		if err != nil {
			log.Fatalf("script: %v", err)
		}
		printJSON(result)

	default:
		fmt.Fprintf(os.Stderr, "unknown script command %q\n", args[0])
		os.Exit(2)
	}
}

func cmdMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	userID := fs.String("user", "", "user id (default: $AUTOCHROME_USER or default)")
	chrome := fs.String("chrome", "/usr/bin/google-chrome", "Chrome executable")
	allowEval := fs.Bool("allow-eval", false, "force-enable browser_eval")
	allowScripts := fs.Bool("allow-scripts", false, "force-enable script tools")
	vnc := fs.Bool("vnc", false, "start x11vnc + noVNC for human takeover")
	vncPassword := fs.String("vnc-password", "", "optional VNC password (also $AUTOCHROME_VNC_PASSWORD)")
	vncStart := fs.Int("vnc-port-start", 0, "override first port for auto-assigned VNC/noVNC ports")
	vncEnd := fs.Int("vnc-port-end", 0, "override last port for auto-assigned VNC/noVNC ports")
	_ = fs.Parse(args)

	u, err := users.Resolve(*userID)
	if err != nil {
		log.Fatal(err)
	}
	srv := mcpserver.New(u, *chrome)
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["allow-eval"] {
		srv.AllowEval = *allowEval
	}
	if set["allow-scripts"] {
		srv.AllowScripts = *allowScripts
	}

	if *vnc {
		d, err := startDesktop(u, *vncPassword, *vncStart, *vncEnd)
		if err != nil {
			log.Fatalf("vnc: %v", err)
		}
		defer d.Close()
		srv.Desktop = d
		log.Printf("vnc: %s (display %s)", d.Info().URL, d.Info().Display)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Fatal(err)
	}
}

func cmdDesktop(args []string) {
	fs := flag.NewFlagSet("desktop", flag.ExitOnError)
	userID := fs.String("user", "", "user id")
	password := fs.String("vnc-password", "", "optional VNC password (also $AUTOCHROME_VNC_PASSWORD)")
	portStart := fs.Int("port-start", 0, "override first port for auto-assigned VNC/noVNC ports")
	portEnd := fs.Int("port-end", 0, "override last port for auto-assigned VNC/noVNC ports")
	_ = fs.Parse(args)

	u, err := users.Resolve(*userID)
	if err != nil {
		log.Fatal(err)
	}
	d, err := startDesktop(u, *password, *portStart, *portEnd)
	if err != nil {
		log.Fatalf("desktop: %v", err)
	}
	defer d.Close()

	printJSON(d.Info())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

func startDesktop(u users.User, password string, portStart, portEnd int) (*desktop.Desktop, error) {
	if password == "" {
		password = os.Getenv("AUTOCHROME_VNC_PASSWORD")
	}
	start, end := u.VNCRange()
	if portStart > 0 {
		start = portStart
	}
	if portEnd > 0 {
		end = portEnd
	}
	if start > 0 {
		u.VNCPortStart = start
	}
	if end > 0 {
		u.VNCPortEnd = end
	}
	stateDir := filepath.Join(u.Home, "data", "users", u.ID)
	return desktop.Start(desktop.Config{
		Screen:    screen,
		PortStart: start,
		PortEnd:   end,
		Password:  password,
		StatePath: filepath.Join(stateDir, "desktop.json"),
		LogDir:    u.LogRoot,
	})
}

// splitPositional extracts a leading positional argument so flags may follow
// it (the standard flag package stops at the first non-flag).
func splitPositional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatalf("encode json: %v", err)
	}
	fmt.Println(string(b))
}

// runUnderXvfb re-executes this command inside xvfb-run when no X display is
// available. xvfb-run exports DISPLAY to the child, so this runs only once.
func runUnderXvfb() {
	if os.Getenv("DISPLAY") != "" {
		return
	}
	bin, err := exec.LookPath("xvfb-run")
	if err != nil {
		log.Fatalf("no DISPLAY and xvfb-run not found; run ./install.sh")
	}
	self, err := os.Executable()
	if err != nil {
		log.Fatalf("resolve executable: %v", err)
	}
	all := append([]string{"-a", "-s", "-screen 0 " + screen, self}, os.Args[1:]...)
	cmd := exec.Command(bin, all...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("xvfb-run: %v", err)
	}
	os.Exit(0)
}
