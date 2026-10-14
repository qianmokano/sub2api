package kanoagent

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var jobPattern = regexp.MustCompile(`^up-[a-f0-9]{32}$`)

type Executor interface {
	CurrentVersion(context.Context) (string, error)
	Execute(context.Context, *Store, *Job) error
}

type Server struct {
	Store    *Store
	Source   ReleaseSource
	Executor Executor
	Wake     chan struct{}
	mu       sync.Mutex
	latest   *Release
	latestAt time.Time
}

func NewServer(store *Store, source ReleaseSource, executor Executor) *Server {
	return &Server{Store: store, Source: source, Executor: executor, Wake: make(chan struct{}, 1)}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, 400, "Invalid updater request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, 400, "Invalid updater request")
		return false
	}
	return true
}

func (s *Server) cachedRelease(ctx context.Context, force bool) (*Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && s.latest != nil && time.Since(s.latestAt) < 20*time.Minute {
		return s.latest, nil
	}
	release, err := s.Source.Latest(ctx)
	if err != nil {
		return s.latest, err
	}
	s.latest = release
	s.latestAt = time.Now()
	return release, nil
}

func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		job, err := s.Store.Current(r.Context())
		if err != nil && !errors.Is(err, ErrNotFound) {
			writeError(w, 503, "Updater state is unavailable")
			return
		}
		writeJSON(w, 200, Availability{AgentVersion: AgentVersion, ProtocolVersion: ProtocolVersion, Ready: true, Busy: job != nil && job.Status.Active()})
	})
	mux.HandleFunc("GET /release", func(w http.ResponseWriter, r *http.Request) {
		current, err := s.Executor.CurrentVersion(r.Context())
		if err != nil {
			writeError(w, 503, "Deployment is unavailable")
			return
		}
		release, releaseErr := s.cachedRelease(r.Context(), r.URL.Query().Get("force") == "true")
		availability := Availability{AgentVersion: AgentVersion, ProtocolVersion: ProtocolVersion, Ready: true, CurrentVersion: current, Release: release}
		if releaseErr != nil {
			availability.Warning = "Release check unavailable; showing the last verified release"
		}
		job, err := s.Store.Current(r.Context())
		if err != nil && !errors.Is(err, ErrNotFound) {
			writeError(w, 503, "Updater state is unavailable")
			return
		}
		availability.Busy = job != nil && job.Status.Active()
		writeJSON(w, 200, availability)
	})
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		var request StartRequest
		if !decodeRequest(w, r, &request) {
			return
		}
		if !ValidReleaseVersion(request.Version) || !ValidDigest(request.ExpectedDigest) {
			writeError(w, 400, "Invalid update target")
			return
		}
		if existing, lookupErr := s.Store.FindRequest(r.Context(), request); lookupErr == nil {
			session, sessionErr := s.session(r.Context(), existing)
			if sessionErr != nil {
				writeError(w, 503, "Updater progress is unavailable")
				return
			}
			writeJSON(w, http.StatusAccepted, session)
			return
		} else if !errors.Is(lookupErr, ErrNotFound) {
			status := http.StatusServiceUnavailable
			if errors.Is(lookupErr, ErrIdempotencyConflict) {
				status = http.StatusConflict
			}
			writeError(w, status, "Update request could not be replayed")
			return
		}
		// Use the agent's own verified release, never a manifest from the caller.
		release, err := s.cachedRelease(r.Context(), false)
		if err != nil || release == nil {
			writeError(w, 503, "Release check unavailable")
			return
		}
		if release.Tag != request.Version || release.Manifest == nil {
			writeError(w, 409, "Check the latest supported fork release before upgrading")
			return
		}
		current, err := s.Executor.CurrentVersion(r.Context())
		if err != nil {
			writeError(w, 503, "Deployment is unavailable")
			return
		}
		job, created, err := s.Store.Create(r.Context(), request, current, *release.Manifest)
		if err != nil {
			status := 400
			if errors.Is(err, ErrBusy) || errors.Is(err, ErrIdempotencyConflict) {
				status = 409
			}
			writeError(w, status, err.Error())
			return
		}
		if created {
			select {
			case s.Wake <- struct{}{}:
			default:
			}
		}
		session, err := s.session(r.Context(), job)
		if err != nil {
			writeError(w, 503, "Update job was saved; recover its progress from the version menu")
			return
		}
		writeJSON(w, http.StatusAccepted, session)
	})
	mux.HandleFunc("GET /jobs/current", func(w http.ResponseWriter, r *http.Request) {
		job, err := s.Store.Current(r.Context())
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, 200, map[string]any{"job": nil})
			return
		}
		if err != nil {
			writeError(w, 503, "Updater state is unavailable")
			return
		}
		session, err := s.session(r.Context(), job)
		if err != nil {
			writeError(w, 503, "Updater progress is unavailable")
			return
		}
		writeJSON(w, 200, session)
	})
	return mux
}

func (s *Server) session(ctx context.Context, job *Job) (*Session, error) {
	token, err := s.Store.Grant(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	return &Session{Job: job, Capability: token, ProgressURL: "/kano-updates/" + job.ID}, nil
}

// Run resumes interrupted work even when no application process is available.
// The host deployment lock must be held for this server's lifetime.
func (s *Server) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	select {
	case s.Wake <- struct{}{}:
	default:
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Wake:
		case <-ticker.C:
		}
		job, err := s.Store.Current(ctx)
		if err != nil || job == nil || !job.Status.Active() || job.Status == NeedsAttention {
			continue
		}
		operationCtx, cancel := context.WithTimeout(ctx, time.Hour)
		err = s.Executor.Execute(operationCtx, s.Store, job)
		cancel()
		if err != nil {
			latest, getErr := s.Store.Get(context.Background(), job.ID)
			if getErr == nil && latest.Status.Active() && latest.Status != NeedsAttention {
				_ = s.Store.Update(context.Background(), job.ID, NeedsAttention, "Recovery needs administrator attention")
			}
		}
	}
}

//go:embed progress.html
var progressHTML string

func (s *Server) ProgressHandler() http.Handler {
	page := template.Must(template.New("progress").Parse(progressHTML))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet {
			writeError(w, 405, "Only progress viewing is allowed")
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/kano-updates/")
		parts := strings.Split(path, "/")
		if len(parts) > 2 || !jobPattern.MatchString(parts[0]) || (len(parts) == 2 && parts[1] != "status") {
			writeError(w, 404, "Progress not found")
			return
		}
		id := parts[0]
		cookie, err := r.Cookie(CookieName(id))
		if err != nil || !s.Store.Authorized(r.Context(), id, cookie.Value) {
			writeError(w, 403, "Progress access expired; reopen it from the administrator version menu")
			return
		}
		job, err := s.Store.Get(r.Context(), id)
		if err != nil {
			writeError(w, 404, "Progress not found")
			return
		}
		if len(parts) == 2 {
			writeJSON(w, 200, job)
			return
		}
		nonce, err := randomID()
		if err != nil {
			writeError(w, 503, "Progress is temporarily unavailable")
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		var rendered bytes.Buffer
		if err = page.Execute(&rendered, map[string]string{"ID": job.ID, "Nonce": nonce, "Source": job.SourceVersion, "Target": job.TargetVersion}); err != nil {
			writeError(w, 503, "Progress is temporarily unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(rendered.Bytes())
	})
}

// ListenUnix replaces only a stale socket. Mount the parent directory in Docker
// so daemon restarts do not leave the application bound to an obsolete inode.
func ListenUnix(path string, gid int) (net.Listener, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("updater socket path must be absolute")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0o750); err != nil {
		return nil, err
	}
	if err := os.Chown(directory, -1, gid); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing to replace a non-socket updater path")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0o660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err = os.Chown(path, -1, gid); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}
