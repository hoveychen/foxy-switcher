// Equivalent to install.sh / uninstall.sh, but in-process so the Tauri GUI
// doesn't have to shell out. The behaviour must stay byte-compatible with the
// curl-based installer — both flows write the same get-token.sh helper and
// patch the same `apiKeyHelper` key in ~/.claude/settings.json.

use std::{fs, path::PathBuf};

use anyhow::{anyhow, Context, Result};
use serde_json::{json, Value};

use crate::sidecar::data_dir;

const HELPER_BODY: &str = r#"#!/bin/bash
# foxy-switcher apiKeyHelper bridge.
set -u
PORT_FILE="${FOXY_SWITCHER_PORT_FILE:-$HOME/.foxy-switcher/port}"
if [ ! -f "$PORT_FILE" ]; then
  echo "foxy-switcher: server not running (no $PORT_FILE)" >&2
  exit 1
fi
PORT=$(cat "$PORT_FILE")
if ! curl -sS --fail --max-time 30 "http://127.0.0.1:${PORT}/api/token"; then
  echo "foxy-switcher: failed to fetch token from 127.0.0.1:${PORT}" >&2
  exit 1
fi
"#;

fn helper_path() -> Result<PathBuf> {
    Ok(data_dir()?.join("get-token.sh"))
}

fn settings_path() -> Result<PathBuf> {
    let home = dirs::home_dir().ok_or_else(|| anyhow!("home dir unavailable"))?;
    Ok(home.join(".claude").join("settings.json"))
}

pub fn install(_port: u16) -> Result<String> {
    let helper = helper_path()?;
    let dir = helper.parent().unwrap();
    fs::create_dir_all(dir).with_context(|| format!("create {}", dir.display()))?;
    fs::write(&helper, HELPER_BODY).with_context(|| format!("write {}", helper.display()))?;
    set_executable(&helper)?;

    let settings = settings_path()?;
    if let Some(parent) = settings.parent() {
        fs::create_dir_all(parent).ok();
    }
    let mut data: Value = if settings.exists() {
        let text = fs::read_to_string(&settings).unwrap_or_default();
        serde_json::from_str(&text).unwrap_or_else(|_| json!({}))
    } else {
        json!({})
    };
    if !data.is_object() {
        data = json!({});
    }
    data["apiKeyHelper"] = Value::String(helper.to_string_lossy().into_owned());
    let pretty = serde_json::to_string_pretty(&data)? + "\n";
    fs::write(&settings, pretty)
        .with_context(|| format!("write {}", settings.display()))?;

    Ok(format!(
        "installed apiKeyHelper at {}",
        helper.display()
    ))
}

pub fn uninstall(purge: bool) -> Result<String> {
    let helper = helper_path()?;
    let _ = fs::remove_file(&helper);

    let settings = settings_path()?;
    if settings.exists() {
        let text = fs::read_to_string(&settings).unwrap_or_default();
        if let Ok(mut data) = serde_json::from_str::<Value>(&text) {
            if let Some(obj) = data.as_object_mut() {
                obj.remove("apiKeyHelper");
                let pretty = serde_json::to_string_pretty(&data)? + "\n";
                fs::write(&settings, pretty).ok();
            }
        }
    }

    if purge {
        let dir = data_dir()?;
        fs::remove_dir_all(&dir).ok();
        return Ok(format!("uninstalled and purged {}", dir.display()));
    }

    Ok("uninstalled apiKeyHelper hook".to_string())
}

pub fn is_installed() -> Result<bool> {
    let helper = helper_path()?;
    if !helper.exists() {
        return Ok(false);
    }
    let settings = settings_path()?;
    if !settings.exists() {
        return Ok(false);
    }
    let text = fs::read_to_string(&settings).unwrap_or_default();
    let data: Value = serde_json::from_str(&text).unwrap_or_else(|_| json!({}));
    let configured = data
        .get("apiKeyHelper")
        .and_then(|v| v.as_str())
        .unwrap_or("");
    Ok(configured == helper.to_string_lossy())
}

#[cfg(unix)]
fn set_executable(path: &std::path::Path) -> Result<()> {
    use std::os::unix::fs::PermissionsExt;
    let mut perms = fs::metadata(path)?.permissions();
    perms.set_mode(0o755);
    fs::set_permissions(path, perms)?;
    Ok(())
}

#[cfg(not(unix))]
fn set_executable(_path: &std::path::Path) -> Result<()> {
    Ok(())
}
