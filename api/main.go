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
)

func main() {
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
		// Query the engine to get the worker's port and websockify port.
		resp, err := http.Get(engineURL + "/workers/" + url.QueryEscape(workerId))
		if err != nil {
			log.Printf("worker lookup failed for %s: %v", workerId, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": "worker lookup failed: " + err.Error()})
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("worker %s not found or error: %d", workerId, resp.StatusCode)
			w.WriteHeader(resp.StatusCode)
			w.Header().Set("Content-Type", "application/json")
			io.Copy(w, resp.Body) //nolint:errcheck
			return
		}

		var workerInfo struct {
			Port      int `json:"port"`
			VNCWSPort int `json:"vnc_ws_port"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&workerInfo); err != nil {
			log.Printf("failed to parse worker info: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": "failed to parse worker info"})
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
		// Formula: vncWSLocalBase + (worker.Port - basePort) where:
		//   vncWSLocalBase = 6800 (engine/control_api/manager.go)
		//   basePort = 18812 (engine/control_api/manager.go)
		// Proxy directly to websockify on the engine container.
		const vncWSLocalBase = 6800
		const basePort = 18812
		localWSPort := vncWSLocalBase + (workerInfo.Port - basePort)
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
	log.Printf("  proxy  /api/* → %s", engineURL)
	log.Printf("  proxy  /api/vnc/installer → %s", installerVNCURL)
	log.Printf("  static /      ← %s", frontendDir)

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
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
