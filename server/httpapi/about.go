package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"
)

// AboutResponse powers the Settings § About card. Everything here is
// runtime-introspectable from the daemon process — no build-time
// configuration required, so the Tauri sidecar and standalone binary both
// produce identical output.
type AboutResponse struct {
	Version       string `json:"version"`         // module version (vX.Y.Z) or "(devel)"
	Commit        string `json:"commit"`          // VCS revision short SHA, or "" if not built from git
	CommitDirty   bool   `json:"commit_dirty"`    // true if VCS reported uncommitted changes at build time
	BuildTime     string `json:"build_time"`      // RFC3339 from VCS metadata; "" when not embedded
	GoVersion     string `json:"go_version"`      // runtime.Version(), e.g. "go1.23.4"
	OS            string `json:"os"`              // runtime.GOOS
	Arch          string `json:"arch"`            // runtime.GOARCH
	PID           int    `json:"pid"`             // daemon process id
	Port          int    `json:"port"`            // bound HTTP port (matches /healthz)
	DataDir       string `json:"data_dir"`        // resolved ~/.foxy-switcher
	SQLitePath    string `json:"sqlite_path"`     // state.db absolute path
	SQLiteSizeB   int64  `json:"sqlite_size_b"`   // bytes; -1 if stat failed
	StartedAtMS   int64  `json:"started_at_ms"`   // unix millis
	UptimeSeconds int64  `json:"uptime_seconds"`  // now - StartedAt
	// Mode tells the frontend which deployment topology this daemon is
	// running under: "combined" (vault + agent in process), "vault" (no
	// credinject, frontend httpapi behind Bearer middleware), or "agent"
	// (transparent reverse proxy to a remote vault). Settings uses this to
	// render the Vault card differently per mode.
	Mode string `json:"mode"`
	// VaultURL is the upstream the agent forwards to. Empty in
	// combined / vault mode. The Settings page shows this so the user
	// knows which remote vault their agent is talking to.
	VaultURL string `json:"vault_url"`
}

func (s *Server) handleGetAbout(w http.ResponseWriter, _ *http.Request) {
	resp := AboutResponse{
		Version:     "(devel)",
		GoVersion:   runtime.Version(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		PID:         os.Getpid(),
		Port:        s.Port,
		DataDir:     s.DataDir,
		SQLitePath:  filepath.Join(s.DataDir, "state.db"),
		SQLiteSizeB: -1,
		StartedAtMS: s.StartedAt.UnixMilli(),
	}
	if !s.StartedAt.IsZero() {
		resp.UptimeSeconds = int64(time.Since(s.StartedAt).Seconds())
	}
	resp.Mode = s.Mode
	resp.VaultURL = s.VaultURL
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" {
			resp.Version = info.Main.Version
		}
		for _, kv := range info.Settings {
			switch kv.Key {
			case "vcs.revision":
				if len(kv.Value) > 12 {
					resp.Commit = kv.Value[:12]
				} else {
					resp.Commit = kv.Value
				}
			case "vcs.modified":
				resp.CommitDirty = kv.Value == "true"
			case "vcs.time":
				resp.BuildTime = kv.Value
			}
		}
	}
	if st, err := os.Stat(resp.SQLitePath); err == nil {
		resp.SQLiteSizeB = st.Size()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleResetData wipes the daemon's persistent state and exits the
// process. The Tauri shell respawns the sidecar (in attached mode the
// caller knows it owns the daemon — the UI hides the button otherwise),
// which boots into a clean state.
//
// We deliberately do NOT try to keep the process alive after deleting
// state.db: the open *sql.DB connection has the file mmaped on some
// platforms, and "delete + reopen in place" is exactly the kind of subtle
// race that bites months later. A clean exit is simpler and matches the
// "Reset all data" mental model — the user expects a fresh start.
func (s *Server) handleResetData(w http.ResponseWriter, r *http.Request) {
	// Snapshot the file paths up front so we can report them in the activity
	// row before the store is closed.
	dbPath := filepath.Join(s.DataDir, "state.db")
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	portPath := filepath.Join(s.DataDir, "port")

	// Emit the activity event BEFORE wiping the bus's table. The event lives
	// in the in-memory ring as well, so the daemon's last log line survives
	// the SQLite delete (until the process exits, anyway).
	s.Bus.EmitWarn("data.reset", 0, "All data wiped via Settings → Reset")

	// Close the store so SQLite's WAL is flushed and the file handles are
	// released. Removing -wal / -shm too keeps the next boot from picking
	// up half-flushed transactions.
	if err := s.Store.Close(); err != nil {
		// Don't bail — we still want to delete and exit, but log so the user
		// sees what went wrong.
		fmt.Fprintln(os.Stderr, "[reset] store close:", err)
	}
	for _, p := range []string{dbPath, walPath, shmPath, portPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "[reset] rm", p, ":", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)

	// Detach the shutdown so the HTTP response actually flushes to the
	// client before the process exits. 200ms is enough for localhost.
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	}()
}

// Compile-time guard that we used context. (Avoids "imported and not used"
// when handlers happen to not need it.)
var _ = context.Background
