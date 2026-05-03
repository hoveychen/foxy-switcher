// Spawns the Go server as a Tauri sidecar and tracks the port it picks.
//
// We pass `--port=0` so the OS chooses a free TCP port; the server then writes
// that port to ~/.foxy-switcher/port. We poll that file (with a short timeout)
// and stash it in ServerState so the React frontend can fetch /api/* against
// the right address.
//
// Before spawning we first try to attach to a daemon that some other entry
// point already brought up — typically `foxy-switcher tui` running in embedded
// mode or a dev `go run .`. If `~/.foxy-switcher/port` resolves and `/healthz`
// answers 200, we just publish that port to ServerState and skip the spawn.
// This mirrors the TUI's own attach-first logic in server/main.go and avoids
// two daemons racing on the same SQLite file + macOS keychain.
//
// On exit we send SIGTERM (Unix) so the sidecar's signal handler runs its
// graceful shutdown path: release the port file, checkpoint the DB WAL, and
// crucially uninstall the apiKeyHelper hook from ~/.claude/settings.json. A
// plain `child.kill()` (SIGKILL) skips that cleanup, leaving the hook orphaned.
// The sidecar's parent-pid watchdog covers the SIGKILL-the-GUI case. When we
// attached instead of spawning, ChildHandle is empty and shutdown is a no-op
// — the daemon outlives this GUI session, which is the whole point of attach.

use std::{
    io::{Read, Write},
    net::{SocketAddr, TcpStream},
    sync::{Arc, Mutex},
    thread,
    time::{Duration, Instant},
};

use anyhow::{anyhow, Context, Result};
use tauri::{AppHandle, Manager};
use tauri_plugin_shell::{process::CommandChild, ShellExt};

use crate::ServerState;

#[derive(Default)]
pub struct ChildHandle(pub Mutex<Option<CommandChild>>);

pub fn spawn(app: &AppHandle) -> Result<()> {
    // Install the ChildHandle exactly once. Subsequent restarts mutate this
    // shared cell rather than calling app.manage again (which panics on a
    // duplicate type). State retrieval after install always succeeds.
    if app.try_state::<Arc<ChildHandle>>().is_none() {
        app.manage(Arc::new(ChildHandle::default()));
    }
    spawn_into(app)
}

// spawn_into owns the spawn / attach machinery without touching app.manage.
// Both first-launch and restart_daemon route through here.
fn spawn_into(app: &AppHandle) -> Result<()> {
    let data_dir = data_dir()?;
    std::fs::create_dir_all(&data_dir)
        .with_context(|| format!("create data dir {}", data_dir.display()))?;
    let port_file = data_dir.join("port");

    // Attach-first: if another daemon (TUI embedded mode, manual `go run .`)
    // already owns the port file and answers /healthz, reuse it. The
    // ChildHandle stays empty in attach mode — restart is intentionally a
    // no-op for daemons we don't own.
    if let Some(port) = try_attach(&port_file) {
        eprintln!("[sidecar] attaching to existing daemon on port {port}");
        if let Some(state) = app.try_state::<ServerState>() {
            *state.port.lock().unwrap() = Some(port);
        }
        return Ok(());
    }

    // No live daemon — clean any stale port file from a prior crash so our
    // file watcher below doesn't read it before the freshly-spawned sidecar
    // has had a chance to overwrite it.
    let _ = std::fs::remove_file(&port_file);

    // Pass our own PID so the sidecar can self-terminate if we die without
    // running the graceful exit path (SIGKILL, OOM kill, debugger detach, ...).
    // The Rust SIGTERM handler covers polite shutdowns; this watchdog covers
    // the rest.
    let parent_pid = std::process::id().to_string();
    let cmd = app
        .shell()
        .sidecar("foxy-switcher-server")
        .map_err(|e| anyhow!("sidecar lookup: {e}"))?
        .args([
            // The bare server binary now defaults to the TUI; `--server` is
            // the explicit gate that keeps the desktop-callable daemon mode.
            "--server",
            "--port",
            "0",
            "--data-dir",
            data_dir.to_string_lossy().as_ref(),
            "--parent-pid",
            &parent_pid,
        ]);

    let (mut rx, child) = cmd
        .spawn()
        .map_err(|e| anyhow!("spawn sidecar: {e}"))?;

    if let Some(handle) = app.try_state::<Arc<ChildHandle>>() {
        *handle.0.lock().unwrap() = Some(child);
    }

    // Drain stdout/stderr so the OS pipe buffer never fills up. Tauri delivers
    // both as CommandEvent variants; we just log them to the host process.
    let app_for_logs = app.clone();
    tauri::async_runtime::spawn(async move {
        use tauri_plugin_shell::process::CommandEvent;
        while let Some(event) = rx.recv().await {
            match event {
                CommandEvent::Stdout(line) | CommandEvent::Stderr(line) => {
                    let s = String::from_utf8_lossy(&line);
                    eprintln!("[sidecar] {}", s.trim_end());
                }
                CommandEvent::Terminated(payload) => {
                    eprintln!("[sidecar] terminated: code={:?}", payload.code);
                    if let Some(s) = app_for_logs.try_state::<ServerState>() {
                        *s.port.lock().unwrap() = None;
                    }
                    break;
                }
                _ => {}
            }
        }
    });

    // Wait (up to 10s) for the port file to appear, then publish it.
    let app_for_port = app.clone();
    thread::spawn(move || {
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            if let Ok(s) = std::fs::read_to_string(&port_file) {
                if let Ok(port) = s.trim().parse::<u16>() {
                    if let Some(state) = app_for_port.try_state::<ServerState>() {
                        *state.port.lock().unwrap() = Some(port);
                    }
                    eprintln!("[sidecar] listening on port {port}");
                    return;
                }
            }
            if Instant::now() >= deadline {
                eprintln!("[sidecar] timed out waiting for port file");
                return;
            }
            thread::sleep(Duration::from_millis(100));
        }
    });

    Ok(())
}

// restart performs a graceful SIGTERM + respawn. Refuses when the current
// daemon was attached (we don't own its lifecycle, so killing it would harm
// the TUI / manual `go run .` user that started it). Returns the new port,
// or an error string the frontend can show in the disconnect banner.
pub fn restart(app: &AppHandle) -> Result<u16, String> {
    let owned = app
        .try_state::<Arc<ChildHandle>>()
        .map(|h| h.0.lock().unwrap().is_some())
        .unwrap_or(false);
    if !owned {
        return Err("Daemon was attached, not owned — restart it externally.".into());
    }
    shutdown(app);
    if let Some(state) = app.try_state::<ServerState>() {
        *state.port.lock().unwrap() = None;
    }
    spawn_into(app).map_err(|e| format!("respawn: {e}"))?;

    // Block briefly until the spawn watcher writes the new port. The watcher
    // polls every 100ms with a 10s deadline; we tail it for up to 5s here so
    // the frontend's invoke() resolves with a port instead of having to
    // re-poll the state.
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        if let Some(state) = app.try_state::<ServerState>() {
            if let Some(p) = *state.port.lock().unwrap() {
                return Ok(p);
            }
        }
        thread::sleep(Duration::from_millis(100));
    }
    Err("daemon did not publish a port within 5s".into())
}

pub fn shutdown(app: &AppHandle) {
    if let Some(handle) = app.try_state::<Arc<ChildHandle>>() {
        if let Some(child) = handle.0.lock().unwrap().take() {
            #[cfg(unix)]
            {
                // SIGTERM lets the Go server run its deferred cleanup
                // (hook.Uninstall + port file removal). We then poll for the
                // process to actually exit before letting the GUI proceed —
                // without this wait, the GUI's event loop can finish before
                // the orphaned daemon has CPU time to run its signal handler,
                // and the user is left with an apiKeyHelper hook still
                // pointing at a get-token.sh that no longer has a daemon
                // behind it.
                let pid = child.pid();
                eprintln!("[shell] sending SIGTERM to sidecar pid {pid}");
                let kill_rc = unsafe { libc::kill(pid as libc::pid_t, libc::SIGTERM) };
                if kill_rc != 0 {
                    eprintln!(
                        "[shell] kill(SIGTERM, {pid}) failed: {}",
                        std::io::Error::last_os_error()
                    );
                }
                let deadline = Instant::now() + Duration::from_secs(3);
                let mut exited = false;
                while Instant::now() < deadline {
                    // signal 0 is the "is this pid alive?" probe.
                    if unsafe { libc::kill(pid as libc::pid_t, 0) } != 0 {
                        exited = true;
                        break;
                    }
                    thread::sleep(Duration::from_millis(50));
                }
                if exited {
                    eprintln!("[shell] sidecar pid {pid} exited cleanly");
                } else {
                    eprintln!(
                        "[shell] sidecar pid {pid} still alive after 3s; leaking"
                    );
                }
                // Drop the handle without invoking kill(). std::process::Child::Drop
                // is a no-op (intentional Rust design), so the OS process keeps
                // running and processes the SIGTERM.
                std::mem::drop(child);
            }
            #[cfg(not(unix))]
            {
                // No clean equivalent on Windows from a separate process.
                // TerminateProcess will skip the Go-side hook uninstall; the
                // hook will stay in settings.json until the next launch
                // overwrites it. The helper script itself handles a missing
                // port file gracefully so Claude Code won't lock up.
                let _ = child.kill();
            }
        }
    }
}

pub fn data_dir() -> Result<std::path::PathBuf> {
    let home = dirs::home_dir().ok_or_else(|| anyhow!("home dir unavailable"))?;
    Ok(home.join(".foxy-switcher"))
}

// try_attach mirrors server/main.go:pingDaemon — read the port file, then
// confirm the daemon is alive via a one-shot HTTP GET /healthz. We do raw
// TCP/HTTP rather than pulling in reqwest: this is on the GUI launch path,
// must finish in well under a second, and only ever talks to 127.0.0.1.
//
// Returns Some(port) only when the file parses AND /healthz answers 200.
// Anything else (missing file, invalid number, refused connect, timeout,
// non-200 response) is treated as "no daemon" and the caller spawns one.
fn try_attach(port_file: &std::path::Path) -> Option<u16> {
    let raw = std::fs::read_to_string(port_file).ok()?;
    let port: u16 = raw.trim().parse().ok()?;
    if probe_healthz(port) {
        Some(port)
    } else {
        None
    }
}

// One-shot HTTP /healthz probe over a fresh TCP connection. Used by
// try_attach (port-file based discovery) and by rediscover (liveness check
// for an already-cached port). 500ms timeouts cap the worst case so a dead
// loopback port doesn't block the GUI's invoke handler for long.
fn probe_healthz(port: u16) -> bool {
    let addr = SocketAddr::from(([127, 0, 0, 1], port));
    let mut stream = match TcpStream::connect_timeout(&addr, Duration::from_millis(500)) {
        Ok(s) => s,
        Err(_) => return false,
    };
    if stream
        .set_read_timeout(Some(Duration::from_millis(500)))
        .is_err()
        || stream
            .set_write_timeout(Some(Duration::from_millis(500)))
            .is_err()
    {
        return false;
    }
    if stream
        .write_all(b"GET /healthz HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n")
        .is_err()
    {
        return false;
    }
    let mut buf = [0u8; 64];
    let n = match stream.read(&mut buf) {
        Ok(n) => n,
        Err(_) => return false,
    };
    if n == 0 {
        return false;
    }
    // Status line is "HTTP/1.x 200 OK" — match the leading bytes loosely so
    // we don't depend on the exact Go net/http phrasing.
    let head = match std::str::from_utf8(&buf[..n]) {
        Ok(s) => s,
        Err(_) => return false,
    };
    let mut parts = head.split_whitespace();
    let _ = match parts.next() {
        Some(v) => v,
        None => return false,
    };
    matches!(parts.next(), Some("200"))
}

// rediscover_port is the GUI's recovery path for "the daemon I attached to
// got restarted on a different port". Cheap when the cached port is alive
// (one local TCP probe, ~sub-ms); on a dead probe it re-reads
// ~/.foxy-switcher/port and re-runs the /healthz handshake to find whatever
// daemon is alive now, then mirrors it into ServerState so the next
// get_server_port returns the fresh value.
//
// Owned mode is implicitly handled: an alive owned daemon passes the probe
// fast-path; a crashed owned daemon already cleared ServerState in the
// CommandEvent::Terminated handler, so we fall through to the port-file
// branch and either find a successor or return None (the caller surfaces
// "no daemon" up to the disconnect banner).
pub fn rediscover_port(app: &AppHandle) -> Option<u16> {
    let cached = app
        .try_state::<ServerState>()
        .and_then(|s| *s.port.lock().unwrap());
    if let Some(port) = cached {
        if probe_healthz(port) {
            return Some(port);
        }
    }
    let port_file = data_dir().ok()?.join("port");
    let fresh = try_attach(&port_file)?;
    if let Some(state) = app.try_state::<ServerState>() {
        *state.port.lock().unwrap() = Some(fresh);
    }
    eprintln!("[sidecar] rediscovered daemon on port {fresh}");
    Some(fresh)
}
