// foxy-switcher Tauri shell.
//
// Responsibilities:
//   1. Spawn the Go server as a sidecar bound to a random port on 127.0.0.1
//   2. Surface the chosen port to the React frontend via `get_server_port`
//   3. Provide a system tray that keeps the sidecar alive when the window
//      is closed (hide-on-close pattern; quit only via tray menu)
//
// The apiKeyHelper hook lifecycle is owned by the Go sidecar (install on
// startup, uninstall on graceful shutdown), so the Tauri layer no longer has
// any hook commands. SIGTERM in sidecar::shutdown is what gives the sidecar
// a chance to run its cleanup defers — see the comment there.

mod sidecar;

use std::sync::{Arc, Mutex};

use tauri::{
    menu::{Menu, MenuItem},
    tray::TrayIconBuilder,
    AppHandle, Manager, RunEvent, WindowEvent,
};
use tauri_plugin_autostart::{ManagerExt, MacosLauncher};

#[derive(Default)]
struct ServerState {
    port: Mutex<Option<u16>>,
}

// get_server_port returns the port of a daemon that is *currently alive*.
// The frontend caches the returned port across api() calls and only re-
// invokes after a connection-level failure, so paying for one /healthz
// probe per invoke buys us automatic recovery from "attached daemon
// restarted on a different port": when the cached port goes dead,
// sidecar::rediscover_port re-reads ~/.foxy-switcher/port and re-handshakes
// /healthz, transparently swapping ServerState to the new daemon.
#[tauri::command]
fn get_server_port(app: AppHandle) -> Result<u16, String> {
    sidecar::rediscover_port(&app).ok_or_else(|| "server not started yet".to_string())
}

// "owned" → this Tauri process spawned the sidecar. "attached" → another
// daemon (TUI embed, `go run .`, an earlier Tauri instance) was already
// alive on the port file when we started, so spawn() short-circuited.
// Distinguished by whether ChildHandle holds a CommandChild.
#[tauri::command]
fn get_daemon_mode(app: AppHandle) -> &'static str {
    let owned = app
        .try_state::<Arc<sidecar::ChildHandle>>()
        .map(|h| h.0.lock().unwrap().is_some())
        .unwrap_or(false);
    if owned {
        "owned"
    } else {
        "attached"
    }
}

// restart_daemon: SIGTERM the current sidecar and respawn. Only valid when
// we own the child — for attached daemons the disconnect banner shows a
// "the daemon belongs to another process" hint instead of this button.
#[tauri::command]
fn restart_daemon(app: AppHandle) -> Result<u16, String> {
    sidecar::restart(&app)
}

// Launch-at-login wrappers around tauri-plugin-autostart. The frontend
// hits these from Settings; the plugin handles per-platform persistence
// (macOS LaunchAgent .plist, Windows Run key, Linux .desktop autostart).
#[tauri::command]
fn autostart_is_enabled(app: AppHandle) -> Result<bool, String> {
    app.autolaunch().is_enabled().map_err(|e| e.to_string())
}

#[tauri::command]
fn autostart_set(app: AppHandle, enabled: bool) -> Result<(), String> {
    let m = app.autolaunch();
    if enabled {
        m.enable().map_err(|e| e.to_string())
    } else {
        m.disable().map_err(|e| e.to_string())
    }
}

// reveal_data_dir opens ~/.foxy-switcher in the OS file manager (Finder /
// Explorer / nautilus). Backed by tauri-plugin-opener's reveal-in-dir which
// highlights the directory itself, mirroring "Reveal in Finder" semantics.
#[tauri::command]
fn reveal_data_dir(app: AppHandle) -> Result<(), String> {
    use tauri_plugin_opener::OpenerExt;
    let dir = sidecar::data_dir().map_err(|e| e.to_string())?;
    app.opener()
        .reveal_item_in_dir(dir.to_string_lossy().as_ref())
        .map_err(|e| e.to_string())
}

// data_dir_path returns the resolved ~/.foxy-switcher so the Settings page
// can show it without baking platform logic into the React side.
#[tauri::command]
fn data_dir_path() -> Result<String, String> {
    sidecar::data_dir()
        .map(|p| p.to_string_lossy().to_string())
        .map_err(|e| e.to_string())
}

// Hook SIGTERM/SIGINT (Ctrl-C on Windows) into the same exit path the tray
// "Quit" menu uses. Without this, a `kill <pid>` from the parent shell skips
// `RunEvent::ExitRequested`, leaks the sidecar, and prints AppKit teardown
// noise that looks like a crash but isn't.
fn install_signal_handler(handle: AppHandle) {
    tauri::async_runtime::spawn(async move {
        #[cfg(unix)]
        {
            use tokio::signal::unix::{signal, SignalKind};
            let mut term = match signal(SignalKind::terminate()) {
                Ok(s) => s,
                Err(e) => {
                    eprintln!("[shell] install SIGTERM handler failed: {e}");
                    return;
                }
            };
            let mut intr = match signal(SignalKind::interrupt()) {
                Ok(s) => s,
                Err(e) => {
                    eprintln!("[shell] install SIGINT handler failed: {e}");
                    return;
                }
            };
            tokio::select! {
                _ = term.recv() => eprintln!("[shell] SIGTERM received, exiting"),
                _ = intr.recv() => eprintln!("[shell] SIGINT received, exiting"),
            }
        }
        #[cfg(windows)]
        {
            if tokio::signal::ctrl_c().await.is_err() {
                return;
            }
            eprintln!("[shell] Ctrl-C received, exiting");
        }
        handle.exit(0);
    });
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_http::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_notification::init())
        // AppleScript launcher works without bundle codesigning fuss; the
        // empty arg list means autostart launches with no extra flags.
        // Pass --start-minimized so a launchd / Run-key triggered launch can
        // be distinguished from a manual one. The Tauri setup() consults the
        // user's StartMinimized preference and hides the window only when
        // both signals agree.
        .plugin(tauri_plugin_autostart::init(
            MacosLauncher::AppleScript,
            Some(vec!["--start-minimized"]),
        ))
        .manage(ServerState::default())
        .invoke_handler(tauri::generate_handler![
            get_server_port,
            get_daemon_mode,
            restart_daemon,
            autostart_is_enabled,
            autostart_set,
            reveal_data_dir,
            data_dir_path
        ])
        .setup(|app| {
            let handle = app.handle().clone();
            sidecar::spawn(&handle)?;
            install_signal_handler(app.handle().clone());

            // Tray-only launch: when the autostart entry fires, it passes
            // --start-minimized so we know to hide the window. Manual launches
            // never carry this flag, so this never blocks a regular open.
            if std::env::args().any(|a| a == "--start-minimized") {
                if let Some(w) = app.get_webview_window("main") {
                    let _ = w.hide();
                }
            }

            let show_item = MenuItem::with_id(app, "show", "Show window", true, None::<&str>)?;
            let quit_item = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;
            let menu = Menu::with_items(app, &[&show_item, &quit_item])?;

            TrayIconBuilder::new()
                .icon(app.default_window_icon().unwrap().clone())
                .tooltip("foxy-switcher")
                .menu(&menu)
                .on_menu_event(|app, event| match event.id.as_ref() {
                    "show" => {
                        if let Some(w) = app.get_webview_window("main") {
                            let _ = w.show();
                            let _ = w.set_focus();
                        }
                    }
                    "quit" => {
                        // Send SIGTERM to the sidecar BEFORE asking Tauri to
                        // exit. We don't trust the ExitRequested path alone —
                        // with hide-on-close prevent_close on the only window,
                        // app.exit() does not always reach our RunEvent
                        // handler in a state where the sidecar is still
                        // shutdown-able. Calling shutdown here is idempotent
                        // (the ChildHandle take() pattern), so the RunEvent
                        // backup below is harmless.
                        sidecar::shutdown(app);
                        app.exit(0);
                    }
                    _ => {}
                })
                .build(app)?;

            Ok(())
        })
        .on_window_event(|window, event| {
            if let WindowEvent::CloseRequested { api, .. } = event {
                // Hide instead of quit so the sidecar keeps serving the apiKeyHelper.
                let _ = window.hide();
                api.prevent_close();
            }
        })
        .build(tauri::generate_context!())
        .expect("error while running tauri application")
        .run(|app, event| {
            // Belt-and-suspenders: ExitRequested fires for app.exit() and
            // most clean-quit paths; Exit fires unconditionally on actual
            // process exit. Hooking both ensures we send SIGTERM no matter
            // which path the runtime takes. shutdown() is idempotent.
            if matches!(event, RunEvent::ExitRequested { .. } | RunEvent::Exit) {
                sidecar::shutdown(app);
            }
        });
}
