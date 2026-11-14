// Package picker opens native file/folder pick dialogs on the engine
// machine (#1): browsers cannot hand the server real filesystem paths,
// so "Pick files…" must run an OS dialog server-side.
//
// Linux: zenity → kdialog → yad → a plain stdin prompt as last resort.
// Windows: a PowerShell System.Windows.Forms OpenFileDialog wrapped in
// a tiny .ps1 (avoids adding a GUI dependency to the pure-stdlib build).
// macOS (engine-mac build): osascript choose file/folder.
//
// All pickers return absolute paths; the API normalizes them.
package picker

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Available reports whether any picker exists on this machine.
func Available() bool {
	switch runtime.GOOS {
	case "linux":
		return firstCmd(zenityBins()...) != "" || os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	case "windows":
		return true // PowerShell ships with Windows
	case "darwin":
		return true // osascript ships with macOS
	}
	return false
}

// Files opens a multi-file picker; returns absolute paths.
func Files() ([]string, error) {
	switch runtime.GOOS {
	case "linux":
		return linuxDialog("file", true)
	case "windows":
		return windowsDialog(false)
	case "darwin":
		return macDialog(false)
	}
	return nil, nil
}

// Folder opens a folder picker; returns absolute paths of the folder
// (the engine walks it itself).
func Folder() ([]string, error) {
	switch runtime.GOOS {
	case "linux":
		return linuxDialog("folder", false)
	case "windows":
		return windowsDialog(true)
	case "darwin":
		return macDialog(true)
	}
	return nil, nil
}

func zenityBins() []string { return []string{"zenity", "kdialog", "yad"} }

func firstCmd(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// linuxDialog: zenity --file-selection [--multiple] [--directory].
func linuxDialog(kind string, multiple bool) ([]string, error) {
	bin := firstCmd(zenityBins()...)
	if bin == "" {
		return nil, nil // no dialog tool: caller falls back to paste-paths
	}
	args := []string{"--file-selection"}
	if kind == "folder" {
		args = append(args, "--directory")
	}
	if multiple && strings.HasSuffix(bin, "zenity") {
		args = append(args, "--multiple", "--separator=\n")
	}
	if multiple && strings.HasSuffix(bin, "kdialog") {
		args = []string{"--getopenfilenames", "\n"}
	}
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		// cancelled / no X at the moment: not an engine error
		return nil, nil
	}
	return splitPaths(string(out)), nil
}

// windowsDialog runs a PowerShell OpenFileDialog / FolderBrowserDialog.
func windowsDialog(folder bool) ([]string, error) {
	ps := windowsScript(folder)
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Output()
	if err != nil {
		return nil, nil // cancelled / headless service session
	}
	return splitPaths(string(out)), nil
}

func windowsScript(folder bool) string {
	if folder {
		return `(New-Object -ComObject Shell.Application).BrowseForFolder(0,
'Select folder to share',0x40+0x10,0).Self.Path`
	}
	return `Add-Type -AssemblyName System.Windows.Forms | Out-Null
$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Multiselect = $true
$d.Title = 'Select files to share'
$null = $d.ShowDialog()
$d.FileNames -join [Environment]::NewLine`
}

// macDialog: osascript "choose file" (multiple) / "choose folder".
func macDialog(folder bool) ([]string, error) {
	script := `tell application "System Events" to choose file with multiple selections allowed`
	if folder {
		script = `tell application "System Events" to choose folder`
	}
	out, err := exec.Command("osascript", "-e", script).Output()
	if err != nil {
		return nil, nil // cancelled
	}
	return splitPaths(string(out)), nil
}

func splitPaths(out string) []string {
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		l = strings.TrimSpace(l)
		if l != "" {
			paths = append(paths, l)
		}
	}
	return paths
}
