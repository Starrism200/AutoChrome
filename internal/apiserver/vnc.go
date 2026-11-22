package apiserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var vncIdleTimeout = 300 * time.Second

func init() {
	if v := os.Getenv("AUTOCHROME_VNC_IDLE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			vncIdleTimeout = time.Duration(n) * time.Second
		}
	}
}

type vncToken struct {
	Token        string
	WebPort      int
	LastActivity time.Time
}

func (s *Server) newVNCToken(webPort int) string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	s.vncMu.Lock()
	s.cleanupVNCLocked()
	s.vncTokens[token] = &vncToken{Token: token, WebPort: webPort, LastActivity: time.Now()}
	s.vncMu.Unlock()
	return token
}

func (s *Server) rawVNCToken(token string) (*vncToken, bool) {
	s.vncMu.Lock()
	defer s.vncMu.Unlock()
	tk, ok := s.vncTokens[token]
	if !ok {
		return nil, false
	}
	if time.Since(tk.LastActivity) > vncIdleTimeout {
		delete(s.vncTokens, token)
		return nil, false
	}
	return tk, true
}

func (s *Server) touchVNC(token string) bool {
	s.vncMu.Lock()
	defer s.vncMu.Unlock()
	tk, ok := s.vncTokens[token]
	if !ok {
		return false
	}
	if time.Since(tk.LastActivity) > vncIdleTimeout {
		delete(s.vncTokens, token)
		return false
	}
	tk.LastActivity = time.Now()
	return true
}

func (s *Server) cleanupVNCLocked() {
	now := time.Now()
	for token, tk := range s.vncTokens {
		if now.Sub(tk.LastActivity) > vncIdleTimeout {
			delete(s.vncTokens, token)
		}
	}
}

func (s *Server) monitorVNCTokens(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.vncMu.Lock()
			s.cleanupVNCLocked()
			s.vncMu.Unlock()
		}
	}
}

func (s *Server) handleDesktopToken(w http.ResponseWriter, r *http.Request) {
	if s.Desktop == nil {
		writeError(w, http.StatusBadRequest, "vnc_disabled", "VNC is not enabled")
		return
	}
	token := s.newVNCToken(s.Desktop.Info().WebPort)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" {
		host = s.Addr
	}
	page := scheme + "://" + host + "/vnc/" + token + "/vnc.html?autoconnect=1&resize=scale&reconnect=1"
	writeJSON(w, http.StatusOK, map[string]any{
		"token":                token,
		"url":                  page,
		"idle_timeout_seconds": int(vncIdleTimeout.Seconds()),
	})
}

func (s *Server) handleVNC(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if _, ok := s.rawVNCToken(token); !ok {
		writeError(w, http.StatusNotFound, "vnc_token_invalid", "VNC token expired or unknown")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/vnc/"+token)
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		http.Redirect(w, r, "/vnc/"+token+"/vnc.html?autoconnect=1&resize=scale&reconnect=1", http.StatusFound)
		return
	}
	if isWebSocketUpgrade(r) {
		s.serveVNCWebSocket(w, r, token, rest)
		return
	}
	if !s.touchVNC(token) {
		writeError(w, http.StatusNotFound, "vnc_token_invalid", "VNC token expired or unknown")
		return
	}
	tk, _ := s.rawVNCToken(token)
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(tk.WebPort))}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = target.Host
		req.URL.Path = "/" + rest
		req.Host = target.Host
	}
	proxy.ServeHTTP(w, r)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func (s *Server) serveVNCWebSocket(w http.ResponseWriter, r *http.Request, token, rest string) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "hijack_unsupported", "server cannot hijack connections")
		return
	}
	tk, ok := s.rawVNCToken(token)
	if !ok {
		writeError(w, http.StatusNotFound, "vnc_token_invalid", "VNC token expired or unknown")
		return
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(tk.WebPort))
	backend, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, "vnc_connect_failed", err.Error())
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		_ = backend.Close()
		return
	}
	_ = rw.Writer.Flush()

	req := r.Clone(r.Context())
	req.URL.Scheme = ""
	req.URL.Host = ""
	req.URL.Path = "/" + rest
	req.RequestURI = ""
	req.Host = addr
	if err := req.Write(backend); err != nil {
		_ = client.Close()
		_ = backend.Close()
		return
	}
	deadline := time.Now().Add(vncIdleTimeout)
	_ = client.SetDeadline(deadline)
	_ = backend.SetDeadline(deadline)

	done := make(chan struct{}, 2)
	go func() {
		s.copyVNC(backend, rw.Reader, client, backend, token)
		done <- struct{}{}
	}()
	go func() {
		s.copyVNC(client, backend, client, backend, token)
		done <- struct{}{}
	}()
	<-done
	_ = client.Close()
	_ = backend.Close()
	<-done
}

func (s *Server) copyVNC(dst io.Writer, src io.Reader, client, backend net.Conn, token string) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
			if !s.touchVNC(token) {
				return
			}
			deadline := time.Now().Add(vncIdleTimeout)
			_ = client.SetDeadline(deadline)
			_ = backend.SetDeadline(deadline)
		}
		if err != nil {
			return
		}
	}
}
