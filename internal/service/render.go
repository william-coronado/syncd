package service

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// RenderPlist returns the launchd agent definition for spec.
//
// KeepAlive restarts the daemon whenever it exits; ThrottleInterval keeps
// a daemon that cannot start (say, its lock is held) from being relaunched
// more than twice a minute. The daemon is started with --wait-for-remote
// (see cli), so a share that is not mounted yet is waited for in-process
// rather than through that restart loop.
func RenderPlist(spec Spec) string {
	stdout, stderr := LogPaths(spec.LogDir, spec.Name)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by "syncd service install"; rerun it to change, "syncd service uninstall" to remove. -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + xmlEscape(Label(spec.Name)) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, arg := range append([]string{spec.Binary}, spec.Args...) {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(arg))
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>StandardOutPath</key>
  <string>` + xmlEscape(stdout) + `</string>
  <key>StandardErrorPath</key>
  <string>` + xmlEscape(stderr) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// RenderUnit returns the systemd user unit for spec.
//
// There is deliberately no mount dependency: a user unit cannot order
// itself after a system automount, and the daemon's --wait-for-remote
// covers it (touching the share is also what triggers an automount).
func RenderUnit(spec Spec) string {
	args := make([]string, 0, len(spec.Args)+1)
	for _, arg := range append([]string{spec.Binary}, spec.Args...) {
		args = append(args, systemdQuote(arg))
	}
	desc := strings.NewReplacer("\n", " ", "%", "%%").Replace(spec.Description)
	return `# Written by "syncd service install"; rerun it to change, "syncd service uninstall" to remove.
[Unit]
Description=` + desc + `

[Service]
Type=simple
ExecStart=` + strings.Join(args, " ") + `
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`
}

// systemdQuote renders one ExecStart argument: specifier (%) and variable
// ($) characters are doubled so systemd passes them through literally, and
// anything but a plain word is double-quoted with C-style escapes.
func systemdQuote(arg string) string {
	plain := arg != ""
	for _, r := range arg {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-=:,+@", r)) {
			plain = false
			break
		}
	}
	if plain {
		return arg
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$", "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(arg) + `"`
}
