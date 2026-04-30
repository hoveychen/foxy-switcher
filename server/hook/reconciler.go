package hook

// Reconcile converges the apiKeyHelper hook state to match `hasAvailable`:
// installed iff at least one account in the pool is currently usable.
// Idempotent — returns changed=false without touching disk if the desired
// state already holds, so it's cheap to call on a ticker.
//
// Why dynamic install/uninstall: when the pool is empty (bootstrap, before
// the user adds the first account) or every account is rate-limited, the
// helper would return non-zero to Claude Code, which blocks the request
// rather than falling back to native auth. Pulling the hook out of
// settings.json in those windows lets Claude Code keep working with its
// own credentials.
func Reconcile(dataDir string, hasAvailable bool) (changed bool, err error) {
	installed := IsInstalled(dataDir)
	switch {
	case hasAvailable && !installed:
		return true, Install(dataDir)
	case !hasAvailable && installed:
		return true, Uninstall(dataDir)
	}
	return false, nil
}
