# Release signing secrets

`.github/workflows/release.yml` ships signed-and-notarized artifacts when the
right secrets are present. None of the secrets are mandatory — if any one is
missing the corresponding signing/notarization step is skipped automatically
and you still get an unsigned `.pkg` / `.msi` (just with Gatekeeper /
SmartScreen warnings on first launch).

This document walks through getting the certificates and feeding them into
the GitHub repo's Actions secrets.

> Add each secret at **Settings → Secrets and variables → Actions →
> New repository secret** in the GitHub UI.

---

## macOS — Apple Developer ID

You need **two** certificates:

| Certificate                    | Used by                  | Signs              |
| ------------------------------ | ------------------------ | ------------------ |
| Developer ID **Application**   | `codesign` (via Tauri)   | the `.app` bundle  |
| Developer ID **Installer**     | `productsign`            | the `.pkg` wrapper |

Both come from the same Apple Developer Program account ($99/yr, individual
or organization).

### 1. Create the certificates

1. Open <https://developer.apple.com/account/resources/certificates/list>.
2. Click `+` → choose **Developer ID Application** → follow the CSR upload
   flow (Keychain Access → Certificate Assistant → Request a Certificate
   from a Certificate Authority, save to disk, upload).
3. Repeat for **Developer ID Installer**.
4. Download both `.cer` files and double-click each to import into the login
   keychain. The private key created during the CSR step links automatically.

### 2. Export each as a password-protected `.p12`

In **Keychain Access → My Certificates**:

1. Right-click "Developer ID Application: ..." → **Export** → save as
   `app.p12` with a password.
2. Right-click "Developer ID Installer: ..." → **Export** → save as
   `installer.p12` with a password.

You can use the same password for both, but they go into different secrets.

### 3. Find the signing-identity strings

```bash
security find-identity -v -p codesigning
```

You'll see lines like:

```
1) ABCDE12345... "Developer ID Application: Your Name (TEAMID1234)"
2) FGHIJ67890... "Developer ID Installer: Your Name (TEAMID1234)"
```

Copy each full quoted string (without quotes).

### 4. Get the team ID and an App Store Connect API key

- **Team ID** — <https://developer.apple.com/account> → Membership → "Team ID"
  (10 chars).
- **API key** — <https://appstoreconnect.apple.com/access/integrations/api>
  → "+" → role **Developer** (notarization requires this role or higher).
  Download the resulting `AuthKey_<KEYID>.p8` **immediately** — Apple only
  lets you do that once. From the same page, copy the **Key ID** (10 chars)
  and the **Issuer ID** (UUID at the top of the page).

> Notarization used to accept Apple-ID + app-specific-password. We've moved
> to API keys because they don't expire by Apple-ID logout/2FA churn and
> can be scoped per-key. The notarize step in CI uses
> `xcrun notarytool submit --key/--key-id/--issuer`.

### 5. Base64-encode the `.p12` files and `.p8` key

```bash
base64 -i app.p12 | pbcopy        # paste into APPLE_CERTIFICATE
base64 -i installer.p12 | pbcopy  # paste into APPLE_INSTALLER_CERTIFICATE
base64 -i AuthKey_<KEYID>.p8 | pbcopy  # paste into APPLE_API_KEY
```

### 6. Add the secrets

| Secret name                            | Value                                                          |
| -------------------------------------- | -------------------------------------------------------------- |
| `APPLE_CERTIFICATE`                    | base64 of `app.p12`                                            |
| `APPLE_CERTIFICATE_PASSWORD`           | the password you set when exporting `app.p12`                  |
| `APPLE_INSTALLER_CERTIFICATE`          | base64 of `installer.p12`                                      |
| `APPLE_INSTALLER_CERTIFICATE_PASSWORD` | the password you set when exporting `installer.p12`            |
| `APPLE_SIGNING_IDENTITY`               | `Developer ID Application: Your Name (TEAMID1234)`             |
| `APPLE_INSTALLER_SIGNING_IDENTITY`     | `Developer ID Installer: Your Name (TEAMID1234)`               |
| `APPLE_TEAM_ID`                        | the 10-char team id                                            |
| `APPLE_API_KEY`                        | base64 of the `.p8` from step 4                                |
| `APPLE_API_KEY_ID`                     | 10-char Key ID shown next to the key in App Store Connect      |
| `APPLE_API_ISSUER`                     | UUID Issuer ID from App Store Connect → Keys                   |
| `KEYCHAIN_PASSWORD`                    | any throwaway string — used to lock the runner's temp keychain |

> Delete the local `.p12` and `.p8` files after uploading. They are reusable
> — back them up to a password manager — but should not stay in your
> Downloads.

---

## Windows — Authenticode

### 1. Get a code-signing certificate

Buy from any commercial CA. Common picks:

- **Standard OV** (~$200–400/yr, e.g. SSL.com, Sectigo, DigiCert) — works
  immediately but SmartScreen still cold-shoulders new publishers for a few
  weeks until reputation builds.
- **EV** (~$300–600/yr, hardware token or HSM-backed) — bypasses SmartScreen
  warnings on day one. Requires a USB token or an HSM/cloud HSM, which means
  signing in CI gets more involved (out of scope for this doc).

This guide assumes a standard OV cert delivered as a `.pfx` file with a
password.

### 2. Base64-encode the `.pfx`

On macOS / Linux:

```bash
base64 -i cert.pfx | pbcopy        # macOS
base64 -w0 cert.pfx                # Linux (no line wrapping)
```

### 3. Add the secrets

| Secret name                     | Value                          |
| ------------------------------- | ------------------------------ |
| `WINDOWS_CERTIFICATE`           | base64 of `cert.pfx`           |
| `WINDOWS_CERTIFICATE_PASSWORD`  | the password protecting `.pfx` |

---

## Verifying

After uploading the secrets:

1. Cut a tag: `git tag v0.1.0 && git push origin v0.1.0`.
2. Watch the Release workflow at the repo's **Actions** tab.
3. The mac job logs should show the `Import Apple certificates`,
   `Wrap (and sign) .pkg`, and `Notarize and staple .pkg` steps **running**
   (not skipped). The Windows job should run `Sign MSI`.
4. Download the `.pkg` and run `pkgutil --check-signature foxy-switcher_*.pkg`
   — it should print `Status: signed by a developer certificate issued by
   Apple for distribution`.
5. Download the `.msi` and run `signtool verify /pa /v <file>.msi` (in a
   Windows shell) — it should print a chain ending at the CA root.

If a step shows "skipped", the matching secret is empty or unset.

---

## Renewal

- **Apple Developer ID** certificates last 5 years. Re-export `.p12` and
  rotate the secrets when they expire.
- **App Store Connect API keys** don't auto-expire but can be revoked from
  App Store Connect → Users and Access → Keys. Rotate proactively when a
  team member with key access leaves.
- **Windows code-signing certificates** typically last 1–3 years. Plan
  rotation before expiry — already-signed `.msi` files keep verifying past
  expiry as long as they include a timestamp (the workflow always adds one).
