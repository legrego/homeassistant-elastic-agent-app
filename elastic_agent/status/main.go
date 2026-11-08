// Command ha-status-server serves the app's status page through Home Assistant
// ingress. It reports how the app is configured, whether the host journal and
// Elasticsearch or Fleet Server can be reached, and Elastic Agent's own health.
//
// It only reads: the app options, the files the entrypoint writes at startup,
// the agent's configuration, and `elastic-agent status`. Secrets never leave
// the process.
package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFiles embed.FS

func main() {
	log.SetFlags(0)
	log.SetPrefix("[ha-status] ")

	listen := envOr("STATUS_LISTEN", ":8099")
	allowed := parseIPs(envOr("STATUS_ALLOWED_IPS", ingressProxyIP))

	collector := NewCollector(PathsFromEnv())
	srv := &http.Server{
		Addr:              listen,
		Handler:           newHandler(collector, allowed),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	log.Printf("Serving the status page on %s.", listen)
	log.Fatal(srv.ListenAndServe())
}

// ingressProxyIP is the Supervisor's ingress proxy. Home Assistant requires
// ingress servers to refuse every other address.
const ingressProxyIP = "172.30.32.2"

func newHandler(c *Collector, allowed []net.IP) http.Handler {
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		report := c.Report(r.Context(), r.URL.Query().Get("refresh") == "1")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(report)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return securityHeaders(allowOnly(allowed, mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; frame-ancestors 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// allowOnly refuses requests that don't come from an allowed address or from
// inside the container.
func allowOnly(allowed []net.IP, next http.Handler) http.Handler {
	var mu sync.Mutex
	logged := map[string]bool{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if remoteAllowed(r.RemoteAddr, allowed) {
			next.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		if !logged[r.RemoteAddr] && len(logged) < 100 {
			logged[r.RemoteAddr] = true
			log.Printf("Refused a request from %s. The status page is only served through Home Assistant.", r.RemoteAddr)
		}
		mu.Unlock()
		http.Error(w, "Open the status page from Home Assistant.", http.StatusForbidden)
	})
}

func remoteAllowed(remoteAddr string, allowed []net.IP) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, a := range allowed {
		if a.Equal(ip) {
			return true
		}
	}
	return false
}

func parseIPs(list string) []net.IP {
	var ips []net.IP
	for _, s := range strings.Split(list, ",") {
		if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
