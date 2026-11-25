// Command scc-admission-webhook is the mutating admission webhook behind
// kind-ocp's scc-admission layer: it gives a plain kind cluster the one
// OpenShift SCC behavior the Compliance Operator's scanner pods need —
// a UID assigned to pods that run non-root without one. OpenShift's
// SecurityContextConstraints admission does this inside the apiserver;
// kind has no SCCs, so the kubelet rejects such pods instead.
//
// The source lives under assets/ because the container image is built
// at create time from these exact bytes (a digest-pinned multi-stage
// docker build; see internal/layers/sccadmission). In place the files
// are a normal package of this repo, so `go test ./...` unit-tests the
// patch logic against the source that ships (AC-6.2); copied into the
// build context with the go.mod the layer writes, they are a standalone
// stdlib-only module, which is what keeps the webhook image free of
// third-party code — the builder image is the only pinned input.
//
// The server is a plain net/http server, so it handles admission calls
// concurrently (the PoC's python webhook served one request at a time).
// Started with an argument, the binary idles instead of serving: the
// layer's verify probe runs it as its container image, so the probe
// needs no second image and no registry pull.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// The wiring the layer's Deployment provides; see internal/layers/
// sccadmission (render.go), which pins these names against this file
// by test.
const (
	// envRunAsUser carries the UID the layer configured for injection
	// (the scc-admission layer's runAsUser setting).
	envRunAsUser = "SCC_RUN_AS_USER"
	// The TLS secret is mounted at /tls by the Deployment.
	tlsCertPath = "/tls/tls.crt"
	tlsKeyPath  = "/tls/tls.key"
	listenAddr  = ":8443"
)

func main() {
	log.SetFlags(0) // one line per event; the kubelet stamps log lines itself
	if len(os.Args) > 1 {
		runProbeMode()
		return
	}
	uid, err := runAsUserFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           webhookHandler(uid),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Printf("scc-admission-webhook listening on %s (injecting runAsUser %d where unset)", listenAddr, uid)
	if err := srv.ListenAndServeTLS(tlsCertPath, tlsKeyPath); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// runProbeMode idles until the process is stopped. The layer's probe
// starts the binary with an argument precisely so the image the cluster
// already carries can be the probe's container image: no shell in the
// scratch image, no second image, no pull.
func runProbeMode() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}

// runAsUserFromEnv reads and validates the UID to inject. A missing,
// non-numeric, or root UID is a configuration error worth dying for:
// silently injecting 0 would defeat the layer's whole purpose.
func runAsUserFromEnv() (int64, error) {
	raw := os.Getenv(envRunAsUser)
	if raw == "" {
		return 0, errors.New(envRunAsUser + " is not set: the deployment must carry the scc-admission layer's runAsUser setting")
	}
	uid, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errors.New(envRunAsUser + "=" + raw + " is not an integer")
	}
	if uid <= 0 {
		return 0, errors.New(envRunAsUser + " must be a positive (non-root) UID, got " + raw)
	}
	return uid, nil
}

// webhookHandler routes the two paths the wiring knows about:
// /healthz for the readiness probe, /mutate for the apiserver.
func webhookHandler(uid int64) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/mutate", func(w http.ResponseWriter, r *http.Request) {
		serveMutation(w, r, uid)
	})
	return mux
}
