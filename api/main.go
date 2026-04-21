// my5fleet api service
//
// Responsibilities:
//   - Serve the frontend/ static assets (SPA fallback to index.html)
//   - Transparent reverse proxy: /api/* → engine:18810/*
//
// Zero external dependencies — pure Go stdlib.
// Binary is statically linked and runs in a scratch container (~5 MB total image).

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

const apiLogMax = 500

var (
	apiLogMu   sync.Mutex
	apiLogRing []string
)

type apiLogWriter struct{ inner io.Writer }

func (w *apiLogWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		apiLogMu.Lock()
		apiLogRing = append(apiLogRing, line)
		if len(apiLogRing) > apiLogMax {
			apiLogRing = apiLogRing[len(apiLogRing)-apiLogMax:]
		}
		apiLogMu.Unlock()
	}
	return n, err
}

func main() {
	log.SetOutput(&apiLogWriter{inner: os.Stderr})

	engineURL := strings.TrimRight(envOr("ENGINE_URL", "http://engine:18810"), "/")
	installerVNCURL := strings.TrimRight(envOr("INSTALLER_VNC_URL", "http://engine:6799"), "/")
	frontendDir := envOr("FRONTEND_DIR", "./frontend")
	port := envOr("PORT", "17380")

	upstream, err := url.Parse(engineURL)
	if err != nil {
		log.Fatalf("invalid ENGINE_URL %q: %v", engineURL, err)
	}

	installerUpstream, err := url.Parse(installerVNCURL)
	if err != nil {
		log.Fatalf("invalid INSTALLER_VNC_URL %q: %v", installerVNCURL, err)
	}

	rp := buildProxy(upstream)
	installerVNCProxy := buildProxy(installerUpstream)
	fs := spaFileServer(frontendDir)

	mux := http.NewServeMux()

	// Combined dashboard logs: api-local + engine-local streams.
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		apiLines := getAPILogs()
		engineLines := getEngineLogs(engineURL)

		combined := make([]string, 0, apiLogMax)
		for _, line := range engineLines {
			combined = append(combined, "engine-local  | "+line)
		}
		for _, line := range apiLines {
			combined = append(combined, "api-local     | "+line)
		}
		if len(combined) > apiLogMax {
			combined = combined[len(combined)-apiLogMax:]
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(combined); err != nil {
			log.Printf("encode combined logs failed: %v", err)
		}
	})

	// Dedicated installer noVNC WebSocket path.
	// This bypasses the engine control-api process and proxies directly to
	// websockify (:6799), which is started earlier by supervisord.
	mux.HandleFunc("/api/vnc/installer", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/"
		r.URL.RawPath = ""
		installerVNCProxy.ServeHTTP(w, r)
	})

	// Dedicated worker noVNC WebSocket path: /api/vnc/workers/{id}
	// Fetch the worker's websockify port from the engine, then proxy directly.
	// This reduces latency vs routing through the engine's control-api VNC handler.
	mux.HandleFunc("/api/vnc/workers/", func(w http.ResponseWriter, r *http.Request) {
		workerId := strings.TrimPrefix(r.URL.Path, "/api/vnc/workers/")
		if workerId == "" {
			http.NotFound(w, r)
			return
		}
		workerInfo, statusCode, detail, err := lookupWorkerVNCInfo(engineURL, workerId)
		if err != nil {
			// 404/503 are common while a worker is being created; avoid noisy error logs.
			if statusCode != http.StatusNotFound && statusCode != http.StatusServiceUnavailable {
				log.Printf("worker lookup failed for %s: %v", workerId, err)
			}
			w.Header().Set("Content-Type", "application/json")
			if statusCode == 0 {
				statusCode = http.StatusBadGateway
			}
			w.WriteHeader(statusCode)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
			return
		}

		if workerInfo.Port == 0 || workerInfo.VNCWSPort == 0 {
			log.Printf("worker %s has no port or VNC port assigned", workerId)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": "worker has no port or VNC port assigned"})
			return
		}

		// Compute the container-local websockify port.
		// Formula: vncWSLocalBase + (worker.Port - workerPortBase)
		// workerPortBase must match WORKER_PORT_RANGE_START used by the engine.
		const vncWSLocalBase = 6800
		workerPortBase := envOrInt("WORKER_PORT_RANGE_START", 18812)
		localWSPort := vncWSLocalBase + (workerInfo.Port - workerPortBase)
		workerVNCURL := &url.URL{Scheme: "http", Host: fmt.Sprintf("engine:%d", localWSPort)}
		workerVNCProxy := buildProxy(workerVNCURL)
		r.URL.Path = "/"
		r.URL.RawPath = ""
		workerVNCProxy.ServeHTTP(w, r)
	})

	// /api/* → engine (strip the /api prefix before forwarding)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, "/api")
		rp.ServeHTTP(w, r)
	})

	// Everything else → frontend SPA (falls back to index.html for unknown paths)
	mux.Handle("/", fs)

	log.Printf("my5fleet api listening :%s", port)
	log.Printf("  proxy  /api/* -> %s", engineURL)
	log.Printf("  proxy  /api/vnc/installer -> %s", installerVNCURL)
	log.Printf("  static /      <- %s", frontendDir)

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func getAPILogs() []string {
	apiLogMu.Lock()
	defer apiLogMu.Unlock()
	lines := make([]string, len(apiLogRing))
	copy(lines, apiLogRing)
	return lines
}

func getEngineLogs(engineURL string) []string {
	res, err := http.Get(engineURL + "/logs")
	if err != nil {
		return []string{"failed to fetch engine logs: " + err.Error()}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return []string{fmt.Sprintf("engine logs request failed: status=%d", res.StatusCode)}
	}
	var lines []string
	if err := json.NewDecoder(res.Body).Decode(&lines); err != nil {
		return []string{"failed to decode engine logs: " + err.Error()}
	}
	return lines
}

func lookupWorkerVNCInfo(engineURL, workerID string) (struct {
	Port      int `json:"port"`
	VNCWSPort int `json:"vnc_ws_port"`
}, int, string, error) {
	var workerInfo struct {
		Port      int `json:"port"`
		VNCWSPort int `json:"vnc_ws_port"`
	}

	lookupURL := engineURL + "/workers/" + url.QueryEscape(workerID)
	deadline := time.Now().Add(3 * time.Second)
	lastStatus := 0
	lastDetail := "worker lookup failed"

	for {
		resp, err := http.Get(lookupURL)
		if err != nil {
			lastStatus = http.StatusBadGateway
			lastDetail = "worker lookup failed: " + err.Error()
			if time.Now().After(deadline) {
				return workerInfo, lastStatus, lastDetail, err
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastStatus = resp.StatusCode
			if len(body) > 0 {
				lastDetail = strings.TrimSpace(string(body))
			} else {
				lastDetail = fmt.Sprintf("worker lookup failed with status %d", resp.StatusCode)
			}

			if (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable) && time.Now().Before(deadline) {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return workerInfo, lastStatus, lastDetail, fmt.Errorf("lookup status %d", resp.StatusCode)
		}

		err = json.NewDecoder(resp.Body).Decode(&workerInfo)
		_ = resp.Body.Close()
		if err != nil {
			lastStatus = http.StatusInternalServerError
			lastDetail = "failed to parse worker info"
			if time.Now().After(deadline) {
				return workerInfo, lastStatus, lastDetail, err
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}

		return workerInfo, 0, "", nil
	}
}

// buildProxy creates a reverse proxy that rewrites requests to the upstream host.
func buildProxy(upstream *url.URL) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(upstream)
	rp.FlushInterval = -1 // required for SSE passthrough

	// Override Director to also fix the Host header (important for some backends).
	base := rp.Director
	rp.Director = func(req *http.Request) {
		base(req)
		req.Host = upstream.Host
	}

	// Surface upstream errors as clean 502 responses instead of silent empty bodies.
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("proxy error for %s %s: %v", r.Method, r.URL.Path, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"detail": "engine unreachable: " + err.Error()})
	}

	return rp
}

// spaFileServer serves static files and falls back to index.html for any path
// that does not correspond to an existing file (standard SPA behaviour).
func spaFileServer(root string) http.Handler {
	fs := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check whether the requested file actually exists on disk.
		reqPath := strings.TrimPrefix(r.URL.Path, "/")
		if reqPath == "" {
			reqPath = "index.html"
		}
		if _, err := os.Stat(root + "/" + reqPath); os.IsNotExist(err) {
			// Asset-like paths (e.g. .js/.css/.map) should 404, not fallback to index.html.
			// This prevents browser MIME errors for missing module scripts.
			if ext := path.Ext(reqPath); ext != "" {
				http.NotFound(w, r)
				return
			}
			// Unknown route path → serve index.html so SPA router handles it.
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFile(w, r, root+"/index.html")
			return
		}
		if reqPath == "index.html" {
			// Always fetch a fresh SPA shell after redeploys.
			w.Header().Set("Cache-Control", "no-store")
		}
		fs.ServeHTTP(w, r)
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}
