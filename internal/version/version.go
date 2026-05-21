// Package version exposes build-time metadata for the gateway
// binary. The three exported vars (Version, GitCommit, BuildDate)
// are intentionally vars (not consts) so the build can stamp
// them via `-ldflags="-X github.com/kennguy3n/zk-object-fabric/internal/version.Version=..."`.
//
// A binary built without ldflags reports "0.0.0-dev" / "unknown"
// so the endpoint always returns something meaningful in
// developer builds — the build doesn't fail just because someone
// ran `go build` instead of the Dockerfile target.
//
// The HTTP handler at Handler() serves the same triple as JSON so
// orchestrators (Helm, ArgoCD, the Kubernetes readiness probe) can
// match the running pod's binary to a specific git SHA without
// shelling into the container.
package version

import (
	"bytes"
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
)

// Defaults the Dockerfile overrides via -ldflags -X. Keep the
// dev defaults stable across the lifetime of the binary — many
// log aggregators key alerts on the literal "unknown" string so
// flipping a dev build to a different default would break those
// detectors. The Version default keeps semver shape so any
// downstream that parses it (Prometheus version_info, the
// console's build banner) does not have to special-case dev.
var (
	Version   = "0.0.0-dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

// Info is the JSON document returned by the /internal/version
// endpoint. The GoVersion / GoArch / GoOS fields come from
// runtime.* at call time so they reflect the actual binary,
// independent of the ldflags-stamped values.
type Info struct {
	Version   string `json:"version"`
	GitCommit string `json:"git_commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	GoArch    string `json:"go_arch"`
	GoOS      string `json:"go_os"`
}

// Current returns a populated Info value built from the
// ldflags-stamped vars plus the runtime triple. Callers must not
// rely on field-by-field stability across releases — this is the
// version document, additions are non-breaking, but consumers
// that hardcode the schema in tests will catch new fields.
func Current() Info {
	return Info{
		Version:   Version,
		GitCommit: GitCommit,
		BuildDate: BuildDate,
		GoVersion: runtime.Version(),
		GoArch:    runtime.GOARCH,
		GoOS:      runtime.GOOS,
	}
}

// Handler returns an http.Handler that serves Current() as a
// JSON document. The handler accepts every HTTP method so a
// `curl -I` HEAD probe also gets the response shape and
// Content-Length without a 405. A HEAD request returns the same
// headers (including the same Content-Length as GET) but an
// empty body, matching RFC 9110 §9.3.2 — the Content-Length
// reported on HEAD MUST equal what a GET would deliver so
// monitoring tooling that uses HEAD to compute payload size
// (e.g. AWS ALB target-group health checks, ngrok inspector)
// gets the right number without an extra GET round-trip.
//
// The response is cached for 60s in shared caches so a noisy
// readiness probe does not stampede the handler on every poll.
// The body is small (~200 bytes) and JSON-encodes in microseconds,
// so the cache header is a polite gesture rather than a
// performance requirement.
//
// Encoding strategy: we materialise the JSON into a buffer
// before writing headers so the same byte stream backs both the
// Content-Length header and the body. Using
// `json.NewEncoder(w).Encode(...)` directly would write the body
// before Content-Length could be set, which is exactly what
// caused the pre-fix HEAD response to be missing Content-Length.
// The buffer is small enough that this is essentially free
// allocation-wise — Info encodes to ~200 bytes — and the
// strconv.Itoa avoids fmt.Sprintf's reflection overhead.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(Current()); err != nil {
			http.Error(w, "encode version info: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(buf.Bytes())
	})
}

// Path is the conventional mount point for the version endpoint.
// cmd/gateway mounts this on the same mux as /internal/metrics so
// orchestrators can probe both without learning two paths.
const Path = "/internal/version"
