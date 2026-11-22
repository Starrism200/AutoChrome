// Command autochrome creates self-contained browser environment directories
// and launches Google Chrome on a virtual display (Xvfb) with a stable,
// per-environment fingerprint and an optional proxy.
//
// Subcommands:
//
//	gen          create an environment directory and its env.json
//	fingerprint  print the deterministic fingerprint for an environment
//	open         launch Chrome for an environment
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"

	"autochrome/internal/browser"
	"autochrome/internal/envdir"
	"autochrome/internal/fingerprint"
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
  autochrome gen         -id <id> [-platform NAME] [-root profiles]
  autochrome fingerprint -id <id>
  autochrome open        -id <id> [-url URL] [-proxy ADDR] [options]

Run "autochrome <command> -h" for command flags.
`)
	os.Exit(2)
}

func cmdGen(args []string) {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	root := fs.String("root", "profiles", "root directory for environment directories")
	id := fs.String("id", "", "environment id (required)")
	platform := fs.String("platform", "default", "platform name used in the directory name")
	_ = fs.Parse(args)

	if *id == "" {
		log.Fatal("gen: -id is required")
	}
	fp := fingerprint.Generate(*id)
	dir, err := envdir.Create(*root, *platform, *id, fp)
	if err != nil {
		log.Fatalf("create environment: %v", err)
	}
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
	root := fs.String("root", "profiles", "root directory for environment directories")
	id := fs.String("id", "", "environment id (required)")
	platform := fs.String("platform", "default", "platform name used in the directory name")
	chrome := fs.String("chrome", "/usr/bin/google-chrome", "Chrome executable")
	proxy := fs.String("proxy", "", "proxy address, e.g. socks5://127.0.0.1:10801")
	url := fs.String("url", "https://example.com", "page to open")
	headless := fs.Bool("headless", false, "run Chrome headless instead of on the virtual display")
	timeout := fs.Duration("timeout", 180*time.Second, "browser timeout")
	keep := fs.Bool("keep", false, "keep the browser open until Ctrl+C")
	screenshot := fs.String("screenshot", "", "optional screenshot path")
	slotDir := fs.String("slot-dir", "data/browser-slots", "browser concurrency slot directory")
	slotMax := fs.Int("slot-max", 0, "max concurrent browsers (0 uses AUTOCHROME_BROWSER_MAX or 10)")
	xvfb := fs.Bool("xvfb", true, "wrap in xvfb-run when DISPLAY is not set")
	_ = fs.Parse(args)

	if *id == "" {
		log.Fatal("open: -id is required")
	}
	if *xvfb {
		runUnderXvfb()
	}

	fp := fingerprint.Generate(*id)
	dir := filepath.Join(*root, envdir.Name(*platform, *id))
	if meta, ok := envdir.ReadMeta(dir); ok && meta.Fingerprint.UserAgent != "" {
		fp = meta.Fingerprint
	} else if _, err := envdir.Create(*root, *platform, *id, fp); err != nil {
		log.Fatalf("create environment: %v", err)
	}

	ctx, cleanup, err := browser.Launch(browser.Options{
		ChromePath:  *chrome,
		ProfileDir:  dir,
		ProxyAddr:   *proxy,
		Headless:    *headless,
		Timeout:     *timeout,
		Fingerprint: &fp,
		SlotDir:     *slotDir,
		SlotMax:     *slotMax,
	})
	if err != nil {
		log.Fatalf("launch browser: %v", err)
	}
	defer cleanup()

	log.Printf("env=%s proxy=%q headless=%v", dir, *proxy, *headless)
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
		if err := browser.Screenshot(ctx, *screenshot); err != nil {
			log.Printf("screenshot: %v", err)
		} else {
			log.Printf("screenshot: %s", *screenshot)
		}
	}

	if *keep {
		log.Printf("browser kept open; press Ctrl+C to close")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
	}
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
