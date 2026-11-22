// Package ua holds the browser identity used by the automation tools.
package ua

// UserAgent emulates Chrome on Windows 10 x64. The build fields are reduced to
// 0.0.0 while the major version follows the installed Chromium.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"

const (
	Platform    = "Win32"
	ChromeMajor = "156"
)
