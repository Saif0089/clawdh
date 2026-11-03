//go:build windows

package notify

import (
	"strings"
)

// notifyCommand drives Windows' own notification stack through the
// PowerShell that ships with the OS, so there is nothing to install.
//
// Two paths, in order: a real toast through WinRT, which is what
// Windows 10/11 shows for everything else, and — if that throws, as it
// does on older builds or where the WinRT assemblies can't be loaded —
// a tray balloon, which Windows itself renders as a toast. Either way
// the message lands in Notification Center rather than a window nobody
// asked for.
func notifyCommand(body string) (string, []string, error) {
	script := `$ErrorActionPreference = 'Stop'
$title = ` + psString(title) + `
$body = ` + psString(body) + `
try {
  [Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
  $template = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
  $texts = $template.GetElementsByTagName('text')
  $texts.Item(0).AppendChild($template.CreateTextNode($title)) | Out-Null
  $texts.Item(1).AppendChild($template.CreateTextNode($body)) | Out-Null
  $toast = [Windows.UI.Notifications.ToastNotification]::new($template)
  [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe').Show($toast)
} catch {
  Add-Type -AssemblyName System.Windows.Forms
  Add-Type -AssemblyName System.Drawing
  $icon = New-Object System.Windows.Forms.NotifyIcon
  $icon.Icon = [System.Drawing.SystemIcons]::Information
  $icon.Visible = $true
  $icon.ShowBalloonTip(10000, $title, $body, [System.Windows.Forms.ToolTipIcon]::Info)
  Start-Sleep -Seconds 5
  $icon.Dispose()
}`
	return "powershell", []string{"-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script}, nil
}

// psString quotes text as a PowerShell single-quoted literal, where the
// only escape is a doubled quote and nothing else is interpreted.
func psString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
