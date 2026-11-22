// Package maintenance coordinates closing a browser, cleaning a container and
// optionally reopening the same browser session.
package maintenance

import (
	"time"

	"autochrome/internal/cleaner"
	"autochrome/internal/envdir"
	"autochrome/internal/session"
)

// Clean closes the container session (if any), runs the cleaner and optionally
// reopens the browser with the same profile/proxy/Chrome version/URL.
func Clean(mgr *session.Manager, meta envdir.Meta, opts cleaner.Options, restart bool) (cleaner.Result, *session.Session, error) {
	meta.FillPaths()
	existing, hasSession := mgr.SessionForEnv(meta.ID)
	var info session.Info
	if hasSession {
		info = existing.Info()
	}
	if hasSession && !opts.DryRun {
		if err := mgr.Close(existing.ID); err != nil {
			return cleaner.Result{}, nil, err
		}
		time.Sleep(300 * time.Millisecond)
	}

	result, err := cleaner.Container(meta, opts)
	if err != nil {
		return result, nil, err
	}
	if opts.DryRun || !hasSession || !restart {
		return result, nil, nil
	}
	if info.Platform == "" {
		info.Platform = meta.Platform
	}
	newSession, err := mgr.Open(meta.ID, info.Platform, info.URL, info.Proxy, info.Headless, info.ChromeVersion)
	if err != nil {
		return result, nil, err
	}
	return result, newSession, nil
}
