package httpapi

import (
    "net/http"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

func Serve(addr string) {
    mux := http.NewServeMux()
    mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request){ w.Write([]byte("ok")) })
    mux.Handle("/metrics", promhttp.Handler())
    go http.ListenAndServe(addr, mux)
}
