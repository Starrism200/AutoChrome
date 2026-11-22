package browser

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Snapshot is a compact, model-friendly view of the current page.
type Snapshot struct {
	URL   string         `json:"url"`
	Title string         `json:"title"`
	Nodes []SnapshotNode `json:"nodes"`
}

// SnapshotNode is one visible interactive element with a stable ref for the
// lifetime of the snapshot.
type SnapshotNode struct {
	Ref      string `json:"ref"`
	Role     string `json:"role"`
	Name     string `json:"name,omitempty"`
	Tag      string `json:"tag,omitempty"`
	Type     string `json:"type,omitempty"`
	Value    string `json:"value,omitempty"`
	Checked  bool   `json:"checked,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Target identifies an element by snapshot ref, CSS selector or visible text.
type Target struct {
	Ref      string
	Selector string
	Text     string
}

const snapshotJS = `(() => {
  const max = %d;
  const visible = (el) => {
    if (!el) return false;
    const r = el.getBoundingClientRect();
    const s = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none';
  };
  const selector = 'a,button,input,textarea,select,summary,[role="button"],[role="link"],[role="textbox"],[role="checkbox"],[role="menuitem"],[role="tab"],[contenteditable="true"]';
  document.querySelectorAll('[data-ac-ref]').forEach((e) => e.removeAttribute('data-ac-ref'));
  const seen = new Set();
  const nodes = [];
  for (const el of document.querySelectorAll(selector)) {
    if (nodes.length >= max) break;
    if (!visible(el) || seen.has(el)) continue;
    seen.add(el);
    const ref = 'e' + (nodes.length + 1);
    el.setAttribute('data-ac-ref', ref);
    const tag = el.tagName.toLowerCase();
    const role = (el.getAttribute('role') || tag).toLowerCase();
    const name = (el.getAttribute('aria-label') || el.getAttribute('placeholder') || el.getAttribute('name') || el.innerText || el.value || '')
      .replace(/\s+/g, ' ').trim().slice(0, 160);
    const node = {ref, role, name, tag};
    if (typeof el.value === 'string' && el.value) node.value = el.value.slice(0, 160);
    if (el.type) node.type = el.type;
    if (el.checked) node.checked = true;
    if (el.disabled) node.disabled = true;
    nodes.push(node);
  }
  return {url: location.href, title: document.title, nodes};
})()`

// TakeSnapshot annotates visible interactive elements with data-ac-ref and
// returns a compact snapshot.
func TakeSnapshot(ctx context.Context, maxNodes int) (Snapshot, error) {
	if maxNodes <= 0 {
		maxNodes = 300
	}
	return chromedp.Run(ctx, chromedp.Evaluate[Snapshot](fmt.Sprintf(snapshotJS, maxNodes)))
}

// SnapshotText renders a snapshot as newline-delimited text for models.
func SnapshotText(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "url: %s\n", s.URL)
	fmt.Fprintf(&b, "title: %s\n", s.Title)
	for _, n := range s.Nodes {
		fmt.Fprintf(&b, "[%s] %s", n.Ref, n.Role)
		if n.Name != "" {
			fmt.Fprintf(&b, " %q", n.Name)
		}
		if n.Value != "" {
			fmt.Fprintf(&b, " value=%q", n.Value)
		}
		if n.Type != "" {
			fmt.Fprintf(&b, " type=%s", n.Type)
		}
		if n.Checked {
			b.WriteString(" checked")
		}
		if n.Disabled {
			b.WriteString(" disabled")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// RefSelector returns the CSS selector for a snapshot ref.
func RefSelector(ref string) string {
	return fmt.Sprintf(`[data-ac-ref=%q]`, ref)
}

func (t Target) selector() string {
	switch {
	case t.Ref != "":
		return RefSelector(t.Ref)
	case t.Selector != "":
		return t.Selector
	default:
		return ""
	}
}

// Navigate opens url in the current tab.
func Navigate(ctx context.Context, url string) error {
	return chromedp.Do(ctx, chromedp.Navigate(url))
}

// Location returns the current page URL.
func Location(ctx context.Context) (string, error) {
	return chromedp.Run(ctx, chromedp.Location())
}

// Title returns the current page title.
func Title(ctx context.Context) (string, error) {
	return chromedp.Run(ctx, chromedp.Title())
}

// Click clicks a target. Text targets use a trusted Enter key event.
func Click(ctx context.Context, t Target) error {
	if t.Text != "" && t.Ref == "" && t.Selector == "" {
		ok, err := ActivateByText(ctx, t.Text)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no visible element with text %q", t.Text)
		}
		return nil
	}
	sel := t.selector()
	if sel == "" {
		return fmt.Errorf("click needs ref, selector or text")
	}
	return chromedp.Do(ctx, chromedp.ScrollIntoView(chromedp.CSS(sel)), chromedp.Click(chromedp.CSS(sel)))
}

// Type focuses a target and sends text using CDP input events.
func Type(ctx context.Context, t Target, text string) error {
	sel := t.selector()
	if sel == "" {
		return fmt.Errorf("type needs ref or selector")
	}
	return chromedp.Do(ctx,
		chromedp.ScrollIntoView(chromedp.CSS(sel)),
		chromedp.Focus(chromedp.CSS(sel)),
		chromedp.SendKeys(chromedp.CSS(sel), text),
	)
}

// Press sends a key event to the focused element.
func Press(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("press needs a key")
	}
	return chromedp.Do(ctx, chromedp.KeyEvent(normalizeKey(key)))
}

func normalizeKey(key string) string {
	switch key {
	case "Enter", "enter":
		return "\r"
	case "Tab", "tab":
		return "\t"
	case "Escape", "escape", "Esc":
		return "\x1b"
	case "Backspace", "backspace":
		return "\b"
	}
	return key
}

// Eval runs JavaScript in the page and awaits promises.
func Eval(ctx context.Context, expression string) (any, error) {
	return chromedp.Run(ctx, chromedp.Evaluate[any](expression, chromedp.EvalAwaitPromise))
}

// WaitFor waits for a selector or for visible text to appear.
func WaitFor(ctx context.Context, selector, text string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if selector == "" && text == "" {
		return fmt.Errorf("wait needs selector or text")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var ok bool
		var err error
		switch {
		case selector != "":
			ok, err = chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`!!document.querySelector(%q)`, selector)))
		default:
			expr := fmt.Sprintf(`document.body && document.body.innerText.includes(%q)`, text)
			ok, err = chromedp.Run(ctx, chromedp.Evaluate[bool](expr))
		}
		if err == nil && ok {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s%s", selector, text)
}

// CapturePNG captures the viewport (fullPage uses a full-page screenshot).
func CapturePNG(ctx context.Context, fullPage bool) ([]byte, error) {
	if fullPage {
		return chromedp.Run(ctx, chromedp.FullScreenshot(90))
	}
	return chromedp.Run(ctx, chromedp.CaptureScreenshot())
}

// GetCookies returns cookies for the given URLs (all cookies when empty).
func GetCookies(ctx context.Context, urls []string) ([]*network.Cookie, error) {
	return chromedp.Run(ctx, func(ctx context.Context, t *chromedp.Target) ([]*network.Cookie, error) {
		res, err := cdp.Call(ctx, t, network.GetCookies, network.GetCookiesParams{URLs: urls})
		if err != nil {
			return nil, err
		}
		return res.Cookies, nil
	})
}

// Rect is an element rectangle in CSS pixels relative to the page origin.
type Rect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// ElementRect returns the page-relative rectangle of the first match.
func ElementRect(ctx context.Context, selector string) (*Rect, error) {
	expr := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return null;
		const r = el.getBoundingClientRect();
		return {x: r.x + window.scrollX, y: r.y + window.scrollY, width: r.width, height: r.height};
	})()`, selector)
	return chromedp.Run(ctx, chromedp.Evaluate[*Rect](expr))
}

// ElementPNG captures a PNG of the first element matching selector.
func ElementPNG(ctx context.Context, selector string) ([]byte, error) {
	rect, err := ElementRect(ctx, selector)
	if err != nil {
		return nil, err
	}
	if rect == nil || rect.Width <= 0 || rect.Height <= 0 {
		return nil, fmt.Errorf("element not found: %s", selector)
	}
	beyond := true
	return chromedp.Run(ctx, func(ctx context.Context, t *chromedp.Target) ([]byte, error) {
		res, err := cdp.Call(ctx, t, page.CaptureScreenshot, page.CaptureScreenshotParams{
			Format: page.CaptureScreenshotFormatPng,
			Clip: &page.Viewport{
				X: rect.X, Y: rect.Y, Width: rect.Width, Height: rect.Height, Scale: 1,
			},
			CaptureBeyondViewport: &beyond,
		})
		if err != nil {
			return nil, err
		}
		return res.Data, nil
	})
}

// ClickXY clicks at viewport coordinates.
func ClickXY(ctx context.Context, x, y float64) error {
	return chromedp.Do(ctx, chromedp.MouseClickXY(x, y))
}

// Scroll scrolls the page by dx/dy CSS pixels.
func Scroll(ctx context.Context, dx, dy int) error {
	expr := fmt.Sprintf(`window.scrollBy(%d, %d)`, dx, dy)
	_, err := chromedp.Run(ctx, chromedp.Evaluate[any](expr))
	return err
}

// WaitOptions describes the conditions WaitForOptions can wait for.
type WaitOptions struct {
	Selector       string
	HiddenSelector string
	Text           string
	URLContains    string
	CookieName     string
	Timeout        time.Duration
}

// WaitForOptions waits until every provided condition is true.
func WaitForOptions(ctx context.Context, opts WaitOptions) error {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if opts.Selector == "" && opts.HiddenSelector == "" && opts.Text == "" && opts.URLContains == "" && opts.CookieName == "" {
		return fmt.Errorf("wait needs at least one condition")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok := true
		if opts.Selector != "" {
			v, err := chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`!!document.querySelector(%q)`, opts.Selector)))
			if err != nil || !v {
				ok = false
			}
		}
		if ok && opts.HiddenSelector != "" {
			v, err := chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`!document.querySelector(%q)`, opts.HiddenSelector)))
			if err != nil || !v {
				ok = false
			}
		}
		if ok && opts.Text != "" {
			v, err := chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`!!document.body && document.body.innerText.includes(%q)`, opts.Text)))
			if err != nil || !v {
				ok = false
			}
		}
		if ok && opts.URLContains != "" {
			v, err := chromedp.Run(ctx, chromedp.Evaluate[bool](fmt.Sprintf(`location.href.includes(%q)`, opts.URLContains)))
			if err != nil || !v {
				ok = false
			}
		}
		if ok && opts.CookieName != "" {
			cookies, err := GetCookies(ctx, nil)
			if err != nil {
				ok = false
			} else {
				found := false
				for _, c := range cookies {
					if c.Name == opts.CookieName {
						found = true
						break
					}
				}
				ok = found
			}
		}
		if ok {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for conditions")
}

// ChallengeCandidate is a detected interactive challenge.
type ChallengeCandidate struct {
	Type     string `json:"type"`
	Selector string `json:"selector"`
	Name     string `json:"name,omitempty"`
	Tag      string `json:"tag,omitempty"`
}

const detectChallengesJS = `(() => {
  document.querySelectorAll('[data-ac-challenge]').forEach((e) => e.removeAttribute('data-ac-challenge'));
  const out = [];
  const visible = (el) => {
    const r = el.getBoundingClientRect();
    const s = getComputedStyle(el);
    return r.width > 0 && r.height > 0 && s.visibility !== 'hidden' && s.display !== 'none';
  };
  const mark = (el, type) => {
    const id = 'ac-' + type + '-' + (out.length + 1);
    el.setAttribute('data-ac-challenge', id);
    return '[data-ac-challenge="' + id + '"]';
  };
  for (const el of document.querySelectorAll('canvas,img,[class*=qr],[id*=qr],[class*=QR],[id*=QR]')) {
    if (!visible(el)) continue;
    const r = el.getBoundingClientRect();
    if (r.width >= 80 && r.height >= 80 && Math.abs(r.width - r.height) < Math.max(r.width, r.height) * 0.4) {
      out.push({type: 'qr', selector: mark(el, 'qr'), name: (el.alt || el.className || el.id || '').slice(0, 80), tag: el.tagName.toLowerCase()});
      break;
    }
  }
  for (const el of document.querySelectorAll('input')) {
    if (!visible(el)) continue;
    const ac = String(el.autocomplete || '').toLowerCase();
    const name = String(el.name || el.id || el.placeholder || '').toLowerCase();
    if (ac.includes('one-time-code') || /otp|verify|code|验证码/.test(name)) {
      out.push({type: 'otp', selector: mark(el, 'otp'), name: name.slice(0, 80), tag: 'input'});
      break;
    }
  }
  for (const el of document.querySelectorAll('iframe,img,div')) {
    if (!visible(el)) continue;
    const text = (String(el.src || '') + ' ' + String(el.className || '') + ' ' + String(el.id || '') + ' ' + String(el.alt || '')).toLowerCase();
    if (/captcha|recaptcha|hcaptcha|turnstile|验证码/.test(text)) {
      out.push({type: 'captcha', selector: mark(el, 'captcha'), name: text.slice(0, 80), tag: el.tagName.toLowerCase()});
      break;
    }
  }
  return out;
})()`

// DetectChallenges returns heuristic QR/OTP/CAPTCHA candidates.
func DetectChallenges(ctx context.Context) ([]ChallengeCandidate, error) {
	return chromedp.Run(ctx, chromedp.Evaluate[[]ChallengeCandidate](detectChallengesJS))
}

// QRCodePNG captures the first detected QR code as PNG.
func QRCodePNG(ctx context.Context) ([]byte, ChallengeCandidate, error) {
	candidates, err := DetectChallenges(ctx)
	if err != nil {
		return nil, ChallengeCandidate{}, err
	}
	for _, c := range candidates {
		if c.Type != "qr" {
			continue
		}
		buf, err := ElementPNG(ctx, c.Selector)
		if err != nil {
			return nil, c, err
		}
		return buf, c, nil
	}
	return nil, ChallengeCandidate{}, fmt.Errorf("no QR code detected")
}

// Reload reloads the current page.
func Reload(ctx context.Context) error {
	return chromedp.Do(ctx, chromedp.Reload())
}

// Back navigates to the previous history entry.
func Back(ctx context.Context) error {
	return chromedp.Do(ctx, chromedp.NavigateBack())
}

// SetCookies writes cookies into the browser context.
func SetCookies(ctx context.Context, cookies []*network.CookieParam) error {
	_, err := chromedp.Run(ctx, func(ctx context.Context, t *chromedp.Target) (chromedp.Void, error) {
		_, err := cdp.Call(ctx, t, network.SetCookies, network.SetCookiesParams{Cookies: cookies})
		return chromedp.Void{}, err
	})
	return err
}
