// HTTP-over-IPC transport for the desktop frontend.
//
// Why this exists: the React UI used to fetch http://127.0.0.1:<port> directly
// from the webview (via @tauri-apps/plugin-http for REST and a native
// EventSource for the activity stream). Both paths honour the *system proxy*
// (reqwest reads HTTP(S)_PROXY/ALL_PROXY; WKWebView reads the macOS network
// settings). A global-mode VPN / 翻墙 proxy that doesn't exempt 127.0.0.1 in
// its bypass list then funnels loopback traffic into the proxy tunnel, and the
// app can't reach its own sidecar — the symptom Boss hit ("数据拉不到").
//
// Routing every request through these Tauri commands moves the actual socket
// work into Rust, where we build the reqwest client with `.no_proxy()` so the
// loopback request never consults proxy settings. The frontend talks to Rust
// over the IPC bridge (no network), so the webview's proxy-honouring fetch is
// out of the picture entirely.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;
use std::collections::HashMap;

use serde::{Deserialize, Serialize};
use tauri::ipc::Channel;
use tauri::{AppHandle, State};

// ProxyState holds the single no-proxy reqwest client shared by every IPC
// request, plus the registry of live activity-stream tasks so the frontend
// can cancel one by id. The client is async (Tauri commands here are async),
// reused so we don't pay TLS/connector setup per call — though for loopback
// http:// there's no TLS handshake anyway.
pub struct ProxyState {
    client: reqwest::Client,
    next_stream_id: AtomicU64,
    streams: Mutex<HashMap<u64, tauri::async_runtime::JoinHandle<()>>>,
}

impl ProxyState {
    pub fn new() -> Self {
        // no_proxy() makes reqwest ignore HTTP_PROXY/HTTPS_PROXY/ALL_PROXY and
        // any system proxy — the whole point of this module. build() only fails
        // if the TLS backend can't initialise; fall back to the default client
        // (still better than panicking on startup) in that unlikely case.
        let client = reqwest::Client::builder()
            .no_proxy()
            .build()
            .unwrap_or_else(|_| reqwest::Client::new());
        Self {
            client,
            next_stream_id: AtomicU64::new(1),
            streams: Mutex::new(HashMap::new()),
        }
    }
}

#[derive(Deserialize)]
pub struct ApiRequest {
    pub method: String,
    // Path + query only (e.g. "/api/accounts?limit=50"). The port is resolved
    // Rust-side via sidecar::rediscover_port so the frontend never has to keep
    // the loopback origin in sync — and so a restarted daemon on a fresh port
    // is picked up transparently, same recovery the old plugin-http path had.
    pub path: String,
    pub headers: Vec<(String, String)>,
    pub body: Option<String>,
}

#[derive(Serialize)]
pub struct ApiResponse {
    pub status: u16,
    pub headers: Vec<(String, String)>,
    pub body: String,
}

// StreamMsg mirrors the EventSource lifecycle the frontend used to get from a
// native EventSource: `open` once the connection is established (so the UI can
// flip to "live" and cancel any reconnect polling), `event` per activity
// payload, and `closed` when the stream ends or errors (the UI's onerror
// pendant — it starts the polling fallback). Without `open`/`closed` the
// frontend couldn't tell a healthy stream from a dead one over the channel.
#[derive(Clone, Serialize)]
#[serde(tag = "kind", rename_all = "lowercase")]
pub enum StreamMsg {
    Open,
    Event { data: String },
    Closed,
}

// api_request is the REST pendant of the old webview fetch. It returns Ok for
// ANY HTTP response (including 4xx/5xx) so the frontend's `!res.ok` handling
// still fires; it returns Err only for transport-level failures (connect
// refused / reset / no daemon), which the frontend treats as a disconnect and
// retries after re-resolving the port — mirroring the previous catch path.
#[tauri::command]
pub async fn api_request(
    app: AppHandle,
    state: State<'_, ProxyState>,
    req: ApiRequest,
) -> Result<ApiResponse, String> {
    let port = crate::sidecar::rediscover_port(&app)
        .ok_or_else(|| "server not started yet".to_string())?;
    let url = format!("http://127.0.0.1:{}{}", port, req.path);
    let method = reqwest::Method::from_bytes(req.method.as_bytes())
        .map_err(|e| format!("bad method {}: {e}", req.method))?;

    let mut rb = state.client.request(method, &url);
    for (k, v) in &req.headers {
        rb = rb.header(k.as_str(), v.as_str());
    }
    if let Some(body) = req.body {
        rb = rb.body(body);
    }

    let resp = rb.send().await.map_err(|e| e.to_string())?;
    let status = resp.status().as_u16();
    let headers = resp
        .headers()
        .iter()
        .map(|(k, v)| (k.as_str().to_string(), v.to_str().unwrap_or("").to_string()))
        .collect();
    let body = resp.text().await.map_err(|e| e.to_string())?;
    Ok(ApiResponse {
        status,
        headers,
        body,
    })
}

// activity_stream opens the daemon's SSE endpoint over the no-proxy client and
// forwards each `event: activity` payload's `data:` JSON to the frontend over
// a Tauri Channel. Returns a stream id the frontend passes to
// activity_stream_stop on unmount. The task runs on Tauri's async runtime so
// aborting the JoinHandle cancels it at the next `.chunk().await` point —
// immediate even though the Go side only heartbeats every 25s (a blocking read
// loop would lag up to that long before noticing the cancel).
#[tauri::command]
pub async fn activity_stream(
    app: AppHandle,
    state: State<'_, ProxyState>,
    on_event: Channel<StreamMsg>,
) -> Result<u64, String> {
    let port = crate::sidecar::rediscover_port(&app)
        .ok_or_else(|| "server not started yet".to_string())?;
    let client = state.client.clone();
    let id = state.next_stream_id.fetch_add(1, Ordering::SeqCst);

    let handle = tauri::async_runtime::spawn(async move {
        let url = format!("http://127.0.0.1:{port}/api/activity/stream");
        let resp = match client.get(&url).send().await {
            Ok(r) => r,
            // onerror pendant: tell the frontend to start polling.
            Err(_) => {
                let _ = on_event.send(StreamMsg::Closed);
                return;
            }
        };
        if !resp.status().is_success() {
            let _ = on_event.send(StreamMsg::Closed);
            return;
        }
        // Connection established — the onopen pendant.
        if on_event.send(StreamMsg::Open).is_err() {
            return;
        }
        let mut resp = resp;
        let mut buf = String::new();
        loop {
            match resp.chunk().await {
                Ok(Some(bytes)) => {
                    buf.push_str(&String::from_utf8_lossy(&bytes));
                    // SSE frames are separated by a blank line. Drain every
                    // complete frame currently in the buffer; keep the partial
                    // tail for the next chunk.
                    while let Some(idx) = buf.find("\n\n") {
                        let frame: String = buf.drain(..idx + 2).collect();
                        if let Some(data) = parse_activity_frame(&frame) {
                            // send() only errors if the channel is gone; the
                            // stop command aborts us before that, so just bail.
                            if on_event.send(StreamMsg::Event { data }).is_err() {
                                return;
                            }
                        }
                    }
                }
                // Stream ended / network error — onerror pendant.
                Ok(None) | Err(_) => {
                    let _ = on_event.send(StreamMsg::Closed);
                    return;
                }
            }
        }
    });

    state.streams.lock().unwrap().insert(id, handle);
    Ok(id)
}

// activity_stream_stop aborts the task spawned by activity_stream. Idempotent:
// an unknown / already-stopped id is a no-op (the frontend may call it from a
// cleanup that races task self-exit).
#[tauri::command]
pub fn activity_stream_stop(state: State<'_, ProxyState>, id: u64) {
    if let Some(handle) = state.streams.lock().unwrap().remove(&id) {
        handle.abort();
    }
}

// parse_activity_frame extracts the JSON `data:` payload from one SSE frame,
// but only for `event: activity` frames. Heartbeats (": ping"), the initial
// ": ready" comment, and id-only lines are ignored (return None). Multi-line
// `data:` fields are joined with "\n" per the SSE spec, though the daemon only
// ever emits single-line JSON.
fn parse_activity_frame(frame: &str) -> Option<String> {
    let mut is_activity = false;
    let mut data_lines: Vec<&str> = Vec::new();
    for line in frame.lines() {
        if let Some(rest) = line.strip_prefix("event:") {
            if rest.trim() == "activity" {
                is_activity = true;
            }
        } else if let Some(rest) = line.strip_prefix("data:") {
            // SSE allows an optional single leading space after the colon.
            data_lines.push(rest.strip_prefix(' ').unwrap_or(rest));
        }
        // ": ..." comments and "id:" lines fall through and are ignored.
    }
    if is_activity && !data_lines.is_empty() {
        Some(data_lines.join("\n"))
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::parse_activity_frame;

    #[test]
    fn extracts_data_from_activity_frame() {
        let frame = "id: 42\nevent: activity\ndata: {\"id\":42,\"type\":\"login\"}\n\n";
        assert_eq!(
            parse_activity_frame(frame).as_deref(),
            Some("{\"id\":42,\"type\":\"login\"}")
        );
    }

    #[test]
    fn ignores_heartbeat_and_ready_comments() {
        assert_eq!(parse_activity_frame(": ping\n\n"), None);
        assert_eq!(parse_activity_frame(": ready\n\n"), None);
    }

    #[test]
    fn ignores_non_activity_events() {
        let frame = "event: other\ndata: {\"x\":1}\n\n";
        assert_eq!(parse_activity_frame(frame), None);
    }

    #[test]
    fn joins_multiline_data() {
        let frame = "event: activity\ndata: line1\ndata: line2\n\n";
        assert_eq!(parse_activity_frame(frame).as_deref(), Some("line1\nline2"));
    }
}
