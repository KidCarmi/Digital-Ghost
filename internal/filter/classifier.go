// classifier.go implements a fast, lightweight content category classifier.
//
// The classifier runs BEFORE the VLM (in < 1ms) and assigns a ContentClass to each
// frame based on the window's process name, title, and URL. This avoids sending
// entertainment content (YouTube thumbnails, Netflix) to Ollama at all — they get
// a low content weight and are dropped before consuming GPU time.
//
// This classifier does NOT use ML. It uses string matching and domain lookup.
// It is intentionally simple and fast. False positives (classifying work content
// as entertainment) are handled by the engagement scorer's other signals.
package filter

import (
	"strings"
)

// ClassifierInput is the metadata available for classification before VLM inference.
type ClassifierInput struct {
	ProcessName string
	WindowTitle string
	BrowserURL  string
}

// Classify returns the ContentClass for the given input.
// This is O(N) in the number of patterns but N is small (< 200) and all
// comparisons are string operations — target: < 500 microseconds.
func Classify(input ClassifierInput) ContentClass {
	url := strings.ToLower(input.BrowserURL)
	title := strings.ToLower(input.WindowTitle)
	proc := strings.ToLower(input.ProcessName)

	// Check entertainment first — it's the most common "noise" source.
	if isEntertainment(url, title, proc) {
		return ClassEntertainment
	}
	if isSocial(url, title, proc) {
		return ClassSocial
	}
	if isWorkCode(url, title, proc) {
		return ClassWorkCode
	}
	if isWorkDocument(url, title, proc) {
		return ClassWorkDocument
	}
	if isWorkResearch(url, title, proc) {
		return ClassWorkResearch
	}
	if isCommunication(url, title, proc) {
		return ClassCommunication
	}
	if isSystemUI(url, title, proc) {
		return ClassSystemUI
	}
	return ClassUnknown
}

func isEntertainment(url, title, proc string) bool {
	urlDomains := []string{
		"youtube.com", "youtu.be",
		"netflix.com",
		"twitch.tv",
		"hulu.com",
		"disneyplus.com",
		"primevideo.com",
		"spotify.com",
		"soundcloud.com",
		"tiktok.com",
		"reddit.com/r/funny", "reddit.com/r/memes", "reddit.com/r/gaming",
		"imgur.com",
		"9gag.com",
		"buzzfeed.com",
	}
	for _, d := range urlDomains {
		if strings.Contains(url, d) {
			return true
		}
	}

	procs := []string{"vlc", "mpv", "totem", "rhythmbox", "spotify", "steam", "lutris", "heroic"}
	for _, p := range procs {
		if strings.Contains(proc, p) {
			return true
		}
	}

	// Video player window titles.
	titleKeywords := []string{"- youtube", "netflix", "twitch"}
	for _, kw := range titleKeywords {
		if strings.Contains(title, kw) {
			return true
		}
	}

	return false
}

func isSocial(url, title, proc string) bool {
	domains := []string{
		"twitter.com", "x.com",
		"instagram.com",
		"facebook.com",
		"linkedin.com/feed",
		"reddit.com",
		"mastodon.social",
		"bsky.app",
	}
	for _, d := range domains {
		if strings.Contains(url, d) {
			return true
		}
	}
	return false
}

func isWorkCode(url, title, proc string) bool {
	procs := []string{
		"code", "code-insiders", // VS Code
		"vim", "nvim", "neovim",
		"emacs",
		"idea", "goland", "pycharm", "webstorm", "clion", "rider",
		"xcode",
		"eclipse",
		"sublime_text",
		"zed",
		"helix",
		"cursor",
		"gnome-terminal", "konsole", "xterm", "alacritty", "kitty", "wezterm", "iterm2", "windows terminal",
		"bash", "zsh", "fish", "sh",
	}
	for _, p := range procs {
		if strings.Contains(proc, p) {
			return true
		}
	}

	domains := []string{
		"github.com",
		"gitlab.com",
		"bitbucket.org",
		"codereview.chromium.org",
		"gerrit",
		"jsfiddle.net",
		"codepen.io",
		"replit.com",
		"codesandbox.io",
	}
	for _, d := range domains {
		if strings.Contains(url, d) {
			return true
		}
	}

	titleKeywords := []string{".go", ".py", ".rs", ".ts", ".js", ".java", ".c", ".cpp", ".rb", ".sh", "terminal", "console"}
	for _, kw := range titleKeywords {
		if strings.Contains(title, kw) {
			return true
		}
	}

	return false
}

func isWorkDocument(url, title, proc string) bool {
	procs := []string{
		"soffice", "libreoffice",
		"evince", "okular", "zathura", // PDF viewers
		"pages", "numbers", "keynote",
		"word", "excel", "powerpoint", "onenote",
		"obsidian",
		"logseq",
		"notion",
		"typora",
		"zettlr",
	}
	for _, p := range procs {
		if strings.Contains(proc, p) {
			return true
		}
	}

	domains := []string{
		"docs.google.com",
		"sheets.google.com",
		"slides.google.com",
		"drive.google.com",
		"notion.so",
		"notion.site",
		"confluence",
		"sharepoint.com",
		"onedrive.live.com",
	}
	for _, d := range domains {
		if strings.Contains(url, d) {
			return true
		}
	}

	titleKeywords := []string{".pdf", ".docx", ".xlsx", ".md", ".txt", "— notion", "confluence"}
	for _, kw := range titleKeywords {
		if strings.Contains(title, kw) {
			return true
		}
	}

	return false
}

func isWorkResearch(url, title, proc string) bool {
	domains := []string{
		"arxiv.org",
		"scholar.google.com",
		"semanticscholar.org",
		"pubmed.ncbi.nlm.nih.gov",
		"stackoverflow.com",
		"stackexchange.com",
		"news.ycombinator.com",
		"lobste.rs",
		"lwn.net",
		"acm.org",
		"ieee.org",
		"medium.com",
		"substack.com",
		"dev.to",
		"docs.", // Matches docs.golang.org, docs.python.org, etc.
		"wiki.",
		"wikipedia.org",
		"man7.org",
	}
	for _, d := range domains {
		if strings.Contains(url, d) {
			return true
		}
	}
	return false
}

func isCommunication(url, title, proc string) bool {
	procs := []string{
		"slack",
		"discord",
		"teams", "msteams",
		"zoom",
		"telegram-desktop",
		"signal-desktop",
		"thunderbird",
		"evolution",
		"mutt", "neomutt",
	}
	for _, p := range procs {
		if strings.Contains(proc, p) {
			return true
		}
	}

	domains := []string{
		"mail.google.com",
		"outlook.office.com",
		"slack.com",
		"discord.com",
		"teams.microsoft.com",
		"jira.",
		"linear.app",
		"asana.com",
		"trello.com",
	}
	for _, d := range domains {
		if strings.Contains(url, d) {
			return true
		}
	}
	return false
}

func isSystemUI(url, title, proc string) bool {
	procs := []string{
		"nautilus", "thunar", "dolphin", "nemo", "pcmanfm", // File managers
		"gnome-control-center", "systemsettings5",           // System settings
		"gnome-software", "discover",                         // App stores
		"synaptic",
		"gnome-disks",
		"gparted",
		"gnome-system-monitor", "ksysguard",
		"finder",       // macOS
		"explorer.exe", // Windows
	}
	for _, p := range procs {
		if strings.Contains(proc, p) {
			return true
		}
	}
	return false
}
