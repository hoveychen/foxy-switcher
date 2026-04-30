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

use std::sync::Mutex;

use tauri::{
    menu::{Menu, MenuItem},
    tray::TrayIconBuilder,
    AppHandle, Manager, RunEvent, WindowEvent,
};

#[derive(Default)]
struct ServerState {
    port: Mutex<Option<u16>>,
}

#[tauri::command]
fn get_server_port(state: tauri::State<'_, ServerState>) -> Result<u16, String> {
    state
        .port
        .lock()
        .unwrap()
        .ok_or_else(|| "server not started yet".to_string())
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
        .manage(ServerState::default())
        .invoke_handler(tauri::generate_handler![get_server_port])
        .setup(|app| {
            let handle = app.handle().clone();
            sidecar::spawn(&handle)?;
            install_signal_handler(app.handle().clone());

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
