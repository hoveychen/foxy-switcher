package deviceinfo

import (
	"bufio"
	"os"
	"strings"
)

// osVersion reads /etc/os-release and returns PRETTY_NAME, which gives
// a human-friendly distro+version string ("Ubuntu 22.04.4 LTS"). Falls
// back to "" if the file is missing (e.g. inside a minimal container).
func osVersion() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// hardwareModel reads the DMI product_name. Bare-metal machines return
// values like "ThinkPad X1 Carbon Gen 9"; VMs return their hypervisor
// stamp (e.g. "VMware Virtual Platform"). Empty when the kernel doesn't
// expose DMI (containers, embedded boards).
func hardwareModel() string {
	data, err := os.ReadFile("/sys/class/dmi/id/product_name")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
