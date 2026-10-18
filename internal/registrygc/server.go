// Package registrygc is the HTTP handler of the registry-gc sidecar. It runs
// Distribution's garbage collection on request, for the gateway's image GC.
package registrygc

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminUser = "admin"
	maxLog    = 64 << 10
)

// Config configures the handler.
type Config struct {
	HtpasswdFile string // reads the "admin:" line
	ConfigFile   string // /etc/distribution/config.yml
	DataDir      string // /var/lib/registry
	// Run executes a command and returns its combined output. Nil runs the
	// command with exec.CommandContext.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

type response struct {
	BytesFreed int64  `json:"bytesFreed"`
	Log        string `json:"log,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Handler serves POST /gc only.
func Handler(cfg Config) http.Handler {
	if cfg.Run == nil {
		cfg.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		}
	}
	s := &server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /gc", s.gc)
	return mux
}

type server struct {
	cfg Config
	mu  sync.Mutex
}

func (s *server) gc(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry-gc"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.mu.TryLock() {
		writeJSON(w, http.StatusConflict, response{Error: "A garbage collection is already running"})
		return
	}
	defer s.mu.Unlock()

	repos := filepath.Join(s.cfg.DataDir, "docker", "registry", "v2", "repositories")
	if _, err := os.Stat(repos); err != nil {
		if os.IsNotExist(err) {
			slog.Info("Skipped garbage collection, registry holds no repositories")
			writeJSON(w, http.StatusOK, response{})
			return
		}
		slog.Error("Could not inspect registry repositories", "error", err)
		writeJSON(w, http.StatusInternalServerError, response{Error: "Could not inspect the registry storage"})
		return
	}

	before := dirSize(s.cfg.DataDir)
	slog.Info("Started garbage collection", "bytesBefore", before)
	out, err := s.cfg.Run(r.Context(), "registry", "garbage-collect", s.cfg.ConfigFile)
	log := tail(out)
	if err != nil {
		slog.Error("Garbage collection failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, response{BytesFreed: max(before-dirSize(s.cfg.DataDir), 0), Log: log, Error: err.Error()})
		return
	}
	freed := max(before-dirSize(s.cfg.DataDir), 0)
	slog.Info("Finished garbage collection", "bytesFreed", freed)
	writeJSON(w, http.StatusOK, response{BytesFreed: freed, Log: log})
}

// authorized checks the request's basic auth against the htpasswd admin line.
func (s *server) authorized(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	hash := adminHash(s.cfg.HtpasswdFile)
	if hash == "" {
		return false
	}
	// Always compare, so a wrong user name costs the same as a wrong password.
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass))
	return subtle.ConstantTimeCompare([]byte(user), []byte(adminUser)) == 1 && err == nil
}

func adminHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		slog.Error("Could not read htpasswd file", "error", err)
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if hash, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), adminUser+":"); ok {
			return hash
		}
	}
	return ""
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // files may vanish while garbage collection runs
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func tail(b []byte) string {
	if len(b) > maxLog {
		b = b[len(b)-maxLog:]
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, code int, v response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
