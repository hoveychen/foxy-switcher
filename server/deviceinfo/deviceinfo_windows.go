package deviceinfo

import (
	"os/exec"
	"strings"
)

// osVersion shells out to `cmd /c ver` and parses the build string.
// `ver` always ships with cmd.exe, so this works on every supported
// Windows release without depending on PowerShell or WMI. Output looks
// like "Microsoft Windows [Version 10.0.19045.5440]" — we extract the
// bracketed version.
func osVersion() string {
	out, err := exec.Command("cmd", "/c", "ver").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if i := strings.Index(s, "[Version "); i >= 0 {
		rest := s[i+len("[Version "):]
		if j := strings.Index(rest, "]"); j >= 0 {
			return rest[:j]
		}
	}
	return s
}

// hardwareModel queries WMI's Win32_ComputerSystem.Model. Older systems
// still ship `wmic` even though Microsoft deprecated it on Windows 11
// 22H2; on systems where it has been removed this returns "". A future
// improvement would call Get-CimInstance via PowerShell, but that's
// substantially more code for a best-effort field.
func hardwareModel() string {
	out, err := exec.Command("wmic", "csproduct", "get", "name").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "Name") {
			continue
		}
		return line
	}
	return ""
}
