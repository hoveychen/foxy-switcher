// Spawns the Go server as a Tauri sidecar and tracks the port it picks.
//
// We pass `--port=0` so the OS chooses a free TCP port; the server then writes
// that port to ~/.foxy-switcher/port. We poll that file (with a short timeout)
// and stash it in ServerState so the React frontend can fetch /api/* against
// the right address.
//
// On exit we send a kill signal to the child so the port file is released and
// the database WAL is checkpointed cleanly. The sidecar's own SIGTERM handler
// also runs `os.Remove(portFile)` for defence in depth.

use std::{
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
    let data_dir = data_dir()?;
    std::fs::create_dir_all(&data_dir)
        .with_context(|| format!("create data dir {}", data_dir.display()))?;
    // Stale port file from a prior crash would otherwise be picked up before
    // the new server has written its own.
    let port_file = data_dir.join("port");
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

    let handle = ChildHandle(Mutex::new(Some(child)));
    app.manage(Arc::new(handle));

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

pub fn shutdown(app: &AppHandle) {
    if let Some(handle) = app.try_state::<Arc<ChildHandle>>() {
        if let Some(child) = handle.0.lock().unwrap().take() {
            let _ = child.kill();
        }
    }
}

pub fn data_dir() -> Result<std::path::PathBuf> {
    let home = dirs::home_dir().ok_or_else(|| anyhow!("home dir unavailable"))?;
    Ok(home.join(".foxy-switcher"))
}
