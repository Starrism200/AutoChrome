// Package browser holds the shared Chromium/CDP setup: a persistent profile,
// an optional proxy, a consistent per-environment fingerprint, timezone and
// the small UI helpers a browser automation process needs.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"autochrome/internal/browserslot"
	"autochrome/internal/fingerprint"
	"autochrome/internal/ua"
)

type Options struct {
	ChromePath  string
	ProfileDir  string
	ProxyAddr   string
	Headless    bool
	Timeout     time.Duration
	Timezone    string
	Fingerprint *fingerprint.Profile
	SlotDir     string
	SlotMax     int
	SlotWait    time.Duration
}

// Launch starts a browser for one account and returns its context plus a
// cleanup function. The browser is already attached to the profile and proxy;
// callers can immediately use chromedp.Do/Run on the returned context.
func Launch(opts Options) (context.Context, func(), error) {
	if opts.ChromePath == "" {
		opts.ChromePath = "/usr/bin/google-chrome"
	}
	// A browser killed by a deploy/restart can leave stale singleton links;
	// Chrome then starts but CDP never attaches. Remove them before launch.
	if opts.ProfileDir != "" {
		for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
			_ = os.Remove(filepath.Join(opts.ProfileDir, name))
		}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 180 * time.Second
	}
	fp := fingerprint.Profile{}
	if opts.Fingerprint != nil {
		fp = *opts.Fingerprint
	}
	if fp.UserAgent == "" || fp.Width <= 0 {
		fp = fingerprint.Generate(opts.ProfileDir)
	}
	if opts.Timezone == "" {
		opts.Timezone = fp.Timezone
	}
	if opts.Timezone == "" {
		opts.Timezone = "Europe/Paris"
	}
	slotWait := opts.SlotWait
	if slotWait <= 0 {
		slotWait = 10 * time.Minute
	}
	releaseSlot, slotErr := browserslot.Acquire(opts.SlotDir, opts.SlotMax, slotWait)
	if slotErr != nil {
		return nil, nil, slotErr
	}

	cacheDir := filepath.Join("/dev/shm", "autochrome-cache", filepath.Base(opts.ProfileDir))
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		cacheDir = ""
	}
	allocOpts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.ExecPath(opts.ChromePath),
		chromedp.UserDataDir(opts.ProfileDir),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-setuid-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-quic", true),
		chromedp.Flag("force-webrtc-ip-handling-policy", "disable_non_proxied_udp"),
		chromedp.Flag("deny-permission-prompts", true),
		chromedp.UserAgent(fp.UserAgent),
		chromedp.WindowSize(fp.Width, fp.Height),
		chromedp.Flag("lang", fp.Language),
		chromedp.Flag("accept-lang", fp.AcceptLanguage),
		chromedp.Flag("disk-cache-size", "67108864"),
		chromedp.Flag("media-cache-size", "33554432"),
		chromedp.Flag("aggressive-cache-discard", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-crash-reporter", true),
		chromedp.Flag("disable-sync", true),
	}
	if cacheDir != "" {
		allocOpts = append(allocOpts, chromedp.Flag("disk-cache-dir", cacheDir))
	}
	if opts.ProxyAddr != "" {
		allocOpts = append(allocOpts, chromedp.Flag("proxy-server", opts.ProxyAddr))
	}
	if opts.Headless {
		allocOpts = append(allocOpts, chromedp.Flag("headless", "new"))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	baseCtx, cancelBase := context.WithTimeout(allocCtx, opts.Timeout)
	ctx, cancelTab := chromedp.NewContext(baseCtx)
	cleanup := func() {
		cancelTab()
		cancelBase()
		cancelAlloc()
		if cacheDir != "" {
			_ = os.RemoveAll(cacheDir)
		}
		releaseSlot()
	}
	if err := chromedp.Do(ctx, emulateDesktop(fp, opts.Timezone)); err != nil {
		cleanup()
		return nil, nil, err
	}
	return ctx, cleanup, nil
}

func emulateDesktop(fp fingerprint.Profile, timezone string) chromedp.Action[chromedp.Void] {
	return func(ctx context.Context, t *chromedp.Target) (chromedp.Void, error) {
		if timezone != "" {
			if _, err := cdp.Call(ctx, t, emulation.SetTimezoneOverride, emulation.SetTimezoneOverrideParams{TimezoneID: timezone}); err != nil {
				return chromedp.Void{}, err
			}
		}
		_, err := cdp.Call(ctx, t, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{
			Width:             int64(fp.Width),
			Height:            int64(fp.Height),
			ScreenWidth:       int64(fp.Width),
			ScreenHeight:      int64(fp.Height),
			DeviceScaleFactor: fp.PixelRatio,
			Mobile:            false,
		})
		if err != nil {
			return chromedp.Void{}, err
		}
		platformVersion := "10.0.0"
		if strings.Contains(fp.Platform, "Mac") {
			platformVersion = "14.0.0"
		} else if strings.Contains(fp.Platform, "Linux") {
			platformVersion = "6.5.0"
		}
		_, err = cdp.Call(ctx, t, emulation.SetUserAgentOverride, emulation.SetUserAgentOverrideParams{
			UserAgent:      fp.UserAgent,
			AcceptLanguage: fp.AcceptLanguage,
			Platform:       fp.Platform,
			UserAgentMetadata: &emulation.UserAgentMetadata{
				Brands: []*emulation.UserAgentBrandVersion{
					{Brand: "Not_A Brand", Version: "24"},
					{Brand: "Chromium", Version: ua.ChromeMajor},
					{Brand: "Google Chrome", Version: ua.ChromeMajor},
				},
				FullVersionList: []*emulation.UserAgentBrandVersion{
					{Brand: "Not_A Brand", Version: "24.0.0.0"},
					{Brand: "Chromium", Version: "156.0.8078.79"},
					{Brand: "Google Chrome", Version: "156.0.8078.79"},
				},
				Platform:        metaPlatform(fp.Platform),
				PlatformVersion: platformVersion,
				Architecture:    "x86",
				Mobile:          false,
				Bitness:         "64",
			},
		})
		if err != nil {
			return chromedp.Void{}, err
		}
		script := fmt.Sprintf(`(() => {
  try { Object.defineProperty(navigator, 'hardwareConcurrency', {get: () => %d, configurable: true}); } catch (e) {}
  try { Object.defineProperty(navigator, 'deviceMemory', {get: () => %d, configurable: true}); } catch (e) {}
  try {
    const patch = (proto) => {
      if (!proto || !proto.getParameter) return;
      const original = proto.getParameter;
      proto.getParameter = function (p) {
        if (p === 37445) return %q;
        if (p === 37446) return %q;
        return original.call(this, p);
      };
    };
    patch(window.WebGLRenderingContext && WebGLRenderingContext.prototype);
    patch(window.WebGL2RenderingContext && WebGL2RenderingContext.prototype);
  } catch (e) {}
})();`, fp.HardwareConcurrency, fp.DeviceMemory, fp.WebGLVendor, fp.WebGLRenderer)
		_, err = cdp.Call(ctx, t, page.AddScriptToEvaluateOnNewDocument, page.AddScriptToEvaluateOnNewDocumentParams{Source: script})
		return chromedp.Void{}, err
	}
}

func metaPlatform(p string) string {
	switch {
	case strings.Contains(p, "Mac"):
		return "macOS"
	case strings.Contains(p, "Linux"):
		return "Linux"
	default:
		return "Windows"
	}
}

// Screenshot writes a full-page screenshot, creating parent directories.
func Screenshot(ctx context.Context, path string) error {
	buf, err := chromedp.Run(ctx, chromedp.FullScreenshot(90))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}

// SaveCookiesFor writes the cookies for the given URLs as JSON.
func SaveCookiesFor(ctx context.Context, urls []string, path string) error {
	cookies, err := chromedp.Run(ctx, func(ctx context.Context, t *chromedp.Target) ([]*network.Cookie, error) {
		res, err := cdp.Call(ctx, t, network.GetCookies, network.GetCookiesParams{URLs: urls})
		if err != nil {
			return nil, err
		}
		return res.Cookies, nil
	})
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cookies, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// SaveCookies writes the current tiktok.com cookies as JSON.
func SaveCookies(ctx context.Context, path string) error {
	return SaveCookiesFor(ctx, []string{"https://www.tiktok.com", "https://www.tiktok.com/"}, path)
}

func EvalString(ctx context.Context, expr string) (string, error) {
	return chromedp.Run(ctx, chromedp.Evaluate[string](expr))
}

func WaitString(ctx context.Context, timeout time.Duration, expr string) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, err := chromedp.Run(ctx, chromedp.Evaluate[string](expr))
		if err == nil && s != "" {
			return s, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("timeout waiting for value")
}

func WaitSelector(ctx context.Context, sel string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`!!document.querySelector(%q)`, sel)))
		if err == nil && ok {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", sel)
}

func WaitEnabled(ctx context.Context, sel string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		enabled, err := chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`(() => { const el = document.querySelector(%q); return !!el && !el.disabled; })()`, sel)))
		if err == nil && enabled {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s to be enabled", sel)
}

func WaitButtonText(ctx context.Context, text string, timeout time.Duration) error {
	expr := fmt.Sprintf(`(() => {
		const want = %q.toLowerCase();
		return [...document.querySelectorAll('button,[role="button"]')].some(x => ((x.innerText || '').trim().toLowerCase()) === want && x.offsetParent !== null);
	})()`, text)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := chromedp.Run(ctx, chromedp.Evaluate[bool](expr))
		if err == nil && ok {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for button %q", text)
}

const activateAttr = "data-tk-activate"

// Activate focuses a button and presses Enter. This produces a trusted keyboard
// event and works even when an overlay (cookie banner) covers the element.
func Activate(ctx context.Context, sel string) error {
	_ = chromedp.Do(ctx, chromedp.ScrollIntoView(chromedp.CSS(sel)))
	return chromedp.Do(ctx, chromedp.Focus(chromedp.CSS(sel)), chromedp.KeyEvent("\r"))
}

// ActivateByText finds a visible button/link by exact text and activates it.
func ActivateByText(ctx context.Context, text string) (bool, error) {
	expr := fmt.Sprintf(`(() => {
		const want = %q.toLowerCase();
		const els = [...document.querySelectorAll('button,[role="button"],a')];
		const b = els.find(x => ((x.innerText || '').trim().toLowerCase()) === want && x.offsetParent !== null);
		if (!b) return false;
		document.querySelectorAll('[%s]').forEach(e => e.removeAttribute('%s'));
		b.setAttribute('%s', '1');
		b.focus();
		return true;
	})()`, text, activateAttr, activateAttr, activateAttr)
	ok, err := chromedp.Run(ctx, chromedp.Evaluate[bool](expr))
	if err != nil || !ok {
		return ok, err
	}
	sel := fmt.Sprintf(`[%s="1"]`, activateAttr)
	err = Activate(ctx, sel)
	_, _ = chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`(() => {
		document.querySelectorAll('[%s]').forEach(e => e.removeAttribute('%s'));
		return true;
	})()`, activateAttr, activateAttr)))
	return err == nil, err
}

func ClickByText(ctx context.Context, text string) (bool, error) {
	return ActivateByText(ctx, text)
}

func Fill(ctx context.Context, sel, value string) error {
	return chromedp.Do(ctx,
		chromedp.WaitVisible(chromedp.CSS(sel)),
		chromedp.Click(chromedp.CSS(sel)),
		chromedp.SendKeys(chromedp.CSS(sel), value),
	)
}

const acceptCookieJS = `(() => {
  const words = [
    "accept all","allow all","accept cookies","allow cookies","accept","allow","agree","i agree","got it",
    "全部接受","接受全部","接受所有","接受","同意","允许全部","全部允许","允许","我知道了","知道了",
    "aceptar todo","aceptar todas","aceptar cookies","aceptar","permitir todo","permitir","de acuerdo",
    "tout accepter","accepter tout","accepter","autoriser tout","autoriser","j'accepte",
    "alle akzeptieren","alles akzeptieren","akzeptieren","alle zulassen","zulassen","einverstanden",
    "accetta tutto","accetta tutti","accetta","consenti tutto","consenti",
    "aceitar tudo","aceitar todos","aceitar","permitir tudo","permitir",
    "alles accepteren","accepteren","alles toestaan","toestaan",
    "akceptuj wszystko","akceptuj","zezwalaj",
    "принять все","принять","согласен","разрешить все",
    "tümünü kabul et","kabul et","kabul ediyorum","izin ver",
    "قبول الكل","موافق","قبول","السماح",
    "terima semua","terima","setuju","izinkan semua",
    "chấp nhận tất cả","chấp nhận","đồng ý",
    "ยอมรับทั้งหมด","ยอมรับ","ตกลง",
    "すべて同意","すべて受け入れる","同意する","許可",
    "모두 수락","수락","동의","허용",
    "सभी स्वीकार करें","स्वीकार करें","सहमत",
    "acceptera alla","acceptera","godkänn alla",
    "godta alle","godta","aksepter alle",
    "accepter alle","accepter","tillad alle",
    "hyväksy kaikki","hyväksy","salli kaikki",
    "přijmout vše","přijmout","povolit vše",
    "acceptă tot","acceptă","permit toate",
    "összes elfogadása","elfogadás","engedélyezés",
    "αποδοχή όλων","αποδοχή","συμφωνώ",
    "קבל הכל","קבל","אני מסכים",
    "прийняти все","прийняти","погоджуюся"
  ];
  const normalize = (s) => String(s || '').toLowerCase().replace(/\s+/g, ' ').trim();
  const visible = (el) => {
    if (!el) return false;
    const r = el.getBoundingClientRect();
    const st = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && st.visibility !== 'hidden' && st.display !== 'none' && !el.disabled;
  };
  const inCookieContext = (el) => {
    let p = el;
    for (let i = 0; i < 10 && p; i++, p = p.parentElement) {
      const text = (String(p.className || '') + ' ' + String(p.id || '') + ' ' + String(p.tagName || '') + ' ' + String(p.getAttribute && (p.getAttribute('data-e2e') || '') || '')).toLowerCase();
      if (/cookie|consent|privacy|gdpr|onetrust|didomi|usercentrics/.test(text)) return true;
    }
    const root = el.getRootNode && el.getRootNode();
    if (root && root.host) {
      const host = root.host;
      const text = (String(host.className || '') + ' ' + String(host.id || '') + ' ' + String(host.tagName || '')).toLowerCase();
      if (/cookie|consent|privacy|gdpr|onetrust|didomi|usercentrics/.test(text)) return true;
    }
    return false;
  };
  const buttons = [];
  const collect = (root) => {
    try {
      for (const b of root.querySelectorAll('button, [role="button"]')) buttons.push(b);
    } catch (e) {}
    try {
      for (const el of root.querySelectorAll('*')) {
        if (el.shadowRoot) collect(el.shadowRoot);
      }
    } catch (e) {}
  };
  collect(document);
  let best = null;
  let bestScore = 0;
  for (const b of buttons) {
    if (!visible(b)) continue;
    const text = normalize(b.innerText || b.textContent || b.getAttribute('aria-label'));
    if (!text) continue;
    let score = 0;
    for (const w of words) {
      if (text === w) { score = Math.max(score, 8); }
      else if (text.includes(w)) { score = Math.max(score, 5); }
    }
    if (score === 0) continue;
    if (inCookieContext(b)) score += 3;
    if (score > bestScore) {
      best = b;
      bestScore = score;
    }
  }
  if (!best) return false;
  try { best.focus(); } catch (e) {}
  best.click();
  return true;
})()`

func DismissCookieBanner(ctx context.Context) {
	deadline := time.Now().Add(5 * time.Second)
	clicked := false
	for time.Now().Before(deadline) {
		ok, err := chromedp.Run(ctx, chromedp.Evaluate[bool](acceptCookieJS))
		if err == nil && ok {
			clicked = true
			time.Sleep(800 * time.Millisecond)
			continue
		}
		if clicked {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}
