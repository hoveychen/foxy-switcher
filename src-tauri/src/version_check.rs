// Release version check with a 1-day TTL cache.
//
// On first call (or when the cache is older than 24 hours), fetch the latest
// release from the GitHub Releases API and persist the result to
// `<data_dir>/version-check.json`. Subsequent calls within the TTL window
// return the cached result instantly with no network I/O. Pattern mirrors
// claude-fleet/src/version_check.rs.

use serde::{Deserialize, Serialize};
use std::path::PathBuf;

const GITHUB_API_URL: &str =
    "https://api.github.com/repos/hoveychen/foxy-switcher/releases/latest";

const TTL_SECS: u64 = 24 * 60 * 60;

#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct VersionCheckResult {
    pub current_version: String,
    pub latest_version: String,
    pub has_update: bool,
    pub release_url: String,
}

#[derive(Serialize, Deserialize)]
struct CacheFile {
    checked_at: u64,
    latest_version: String,
    release_url: String,
}

#[derive(Deserialize)]
struct GithubRelease {
    tag_name: String,
    html_url: String,
}

fn cache_path() -> Option<PathBuf> {
    crate::sidecar::data_dir().ok().map(|d| d.join("version-check.json"))
}

fn now_secs() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

// Parse "v0.2.0" / "0.2.0" / "0.2.0-rc1" → (major, minor, patch). Pre-release
// tags are stripped so "0.2.0-rc1" compares equal to "0.2.0" — good enough to
// avoid offering a "downgrade" when the user is running an rc of the same
// release that's now GA, and avoids semver dep weight.
fn parse_version(v: &str) -> (u32, u32, u32) {
    let v = v.trim_start_matches('v');
    let base = v.split('-').next().unwrap_or(v);
    let mut parts = base.split('.').filter_map(|p| p.parse::<u32>().ok());
    (
        parts.next().unwrap_or(0),
        parts.next().unwrap_or(0),
        parts.next().unwrap_or(0),
    )
}

fn fetch_latest() -> Result<(String, String), String> {
    let client = reqwest::blocking::Client::builder()
        .timeout(std::time::Duration::from_secs(10))
        .build()
        .map_err(|e| format!("http client: {e}"))?;

    let resp = client
        .get(GITHUB_API_URL)
        .header("User-Agent", "foxy-switcher-version-check")
        .header("Accept", "application/vnd.github+json")
        .send()
        .map_err(|e| format!("fetch: {e}"))?;

    if !resp.status().is_success() {
        return Err(format!("HTTP {}", resp.status()));
    }

    let release: GithubRelease = resp.json().map_err(|e| format!("json: {e}"))?;
    let version = release.tag_name.trim_start_matches('v').to_string();
    Ok((version, release.html_url))
}

fn fetch_and_cache(path: &PathBuf) -> (String, String) {
    match fetch_latest() {
        Ok((version, url)) => {
            let cache = CacheFile {
                checked_at: now_secs(),
                latest_version: version.clone(),
                release_url: url.clone(),
            };
            if let Ok(json) = serde_json::to_string(&cache) {
                if let Some(dir) = path.parent() {
                    let _ = std::fs::create_dir_all(dir);
                }
                let _ = std::fs::write(path, json);
            }
            (version, url)
        }
        Err(e) => {
            eprintln!("[version_check] fetch failed: {e}");
            (String::new(), String::new())
        }
    }
}

// Return version information, using a 1-day cached result when available.
// Never panics; on any error the `latest_version` field is empty and
// `has_update` is false so the frontend simply doesn't render the banner.
//
// `force` bypasses the TTL — the menu's "Check for Updates" and the Settings
// "Check now" button pass true so the user gets a fresh answer.
pub fn check_app_version(force: bool) -> VersionCheckResult {
    let current_version = env!("CARGO_PKG_VERSION").to_string();

    let (latest_version, release_url) = match cache_path() {
        Some(path) => {
            if force {
                fetch_and_cache(&path)
            } else {
                let cached = std::fs::read_to_string(&path)
                    .ok()
                    .and_then(|s| serde_json::from_str::<CacheFile>(&s).ok());
                match cached {
                    Some(c) if now_secs().saturating_sub(c.checked_at) < TTL_SECS => {
                        (c.latest_version, c.release_url)
                    }
                    _ => fetch_and_cache(&path),
                }
            }
        }
        None => fetch_latest().unwrap_or_default(),
    };

    let has_update = !latest_version.is_empty()
        && parse_version(&latest_version) > parse_version(&current_version);

    VersionCheckResult {
        current_version,
        latest_version,
        has_update,
        release_url,
    }
}

#[cfg(test)]
mod tests {
    use super::parse_version;

    #[test]
    fn parses_plain_and_v_prefixed() {
        assert_eq!(parse_version("0.2.0"), (0, 2, 0));
        assert_eq!(parse_version("v0.2.0"), (0, 2, 0));
        assert_eq!(parse_version("v12.34.56"), (12, 34, 56));
    }

    #[test]
    fn strips_pre_release_suffix() {
        // "0.2.0-rc1" must compare equal to "0.2.0" so the banner doesn't
        // suggest a "downgrade" when an rc user lands on the GA release.
        assert_eq!(parse_version("0.2.0-rc1"), (0, 2, 0));
        assert_eq!(parse_version("v1.0.0-beta.3"), (1, 0, 0));
    }

    #[test]
    fn tolerates_short_and_garbage_inputs() {
        // Missing minor/patch components default to 0 — keeps the ord
        // comparison defined for tags like "v1" that drift in by accident.
        assert_eq!(parse_version("1"), (1, 0, 0));
        assert_eq!(parse_version("1.2"), (1, 2, 0));
        // Total garbage falls through to (0,0,0) — never panics, and the
        // caller's has_update guard already filters on non-empty version.
        assert_eq!(parse_version(""), (0, 0, 0));
        assert_eq!(parse_version("not-a-version"), (0, 0, 0));
    }

    #[test]
    fn ordering_drives_has_update_decision() {
        // The has_update branch in check_app_version uses tuple ord on
        // these triples — pin the contract so a future "fix" to
        // parse_version can't silently flip the comparison direction.
        assert!(parse_version("0.2.0") > parse_version("0.1.0"));
        assert!(parse_version("v1.0.0") > parse_version("0.9.99"));
        assert!(!(parse_version("0.1.0") > parse_version("0.1.0")));
    }
}
