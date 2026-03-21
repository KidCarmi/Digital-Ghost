// Package filter provides the privacy blocklist engine and semantic quality filters.
//
// blocklist.go implements the privacy blocklist — the first line of defense that
// prevents sensitive windows from ever being captured.
//
// FAIL-CLOSED CONTRACT:
//   - If the blocklist file cannot be read or parsed, Block() returns true for ALL windows.
//   - There is no "allowlist override" that can bypass a block at runtime.
//   - Hot-reload (via fsnotify) uses atomic in-memory swap; the old blocklist remains
//     active until the new one is fully validated.
package filter

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// BlocklistConfig is the parsed structure of blocklist.yaml.
type BlocklistConfig struct {
	Apps               []string `yaml:"apps"`
	AuthPatterns       []string `yaml:"auth_patterns"`
	URLPatterns        []string `yaml:"url_patterns"`
	TitlePatterns      []string `yaml:"title_patterns"`
	SensitiveInputRoles []string `yaml:"sensitive_input_roles"`
}

// compiledBlocklist holds the pre-compiled regex patterns for fast matching.
type compiledBlocklist struct {
	apps            map[string]struct{} // exact match, case-insensitive key
	authPatterns    map[string]struct{} // exact match
	urlRegexps      []*regexp.Regexp
	titleRegexps    []*regexp.Regexp
	sensitiveInputs map[string]struct{}
}

// WindowContext contains all metadata available about the currently-active window.
// It is populated BEFORE any pixel is read from the screen.
type WindowContext struct {
	// ProcessName is the basename of the process executable (e.g., "1password", "firefox").
	ProcessName string

	// WindowTitle is the full title of the active window.
	WindowTitle string

	// BrowserURL is the URL currently shown in the browser's address bar,
	// obtained via the accessibility API. Empty for non-browser windows.
	BrowserURL string

	// FocusedInputRole is the ARIA role or input type of the currently-focused
	// element (e.g., "password" for <input type="password">). Empty if unknown.
	FocusedInputRole string

	// PID is the OS process ID of the window owner.
	PID int
}

// BlockDecision describes why a frame was blocked or allowed.
type BlockDecision struct {
	Blocked bool
	Reason  string // Human-readable explanation for the capture log.
}

// Blocklist is the hot-reloadable, fail-closed privacy gate.
type Blocklist struct {
	// current is an atomically-swapped pointer to the active compiled blocklist.
	// Using atomic pointer to allow lock-free reads on the hot path.
	current atomic.Pointer[compiledBlocklist]

	// mu protects the reload operation (write path only).
	mu sync.Mutex

	// healthy is false when the blocklist is in fail-closed mode (no valid config loaded).
	healthy bool

	watcher *fsnotify.Watcher
	logger  *slog.Logger
}

// NewBlocklist creates a Blocklist from the given config path and a set of
// hardcoded defaults. The blocklist is fail-closed: if path cannot be read,
// all windows are blocked.
//
// Call Close() when the Blocklist is no longer needed to stop the file watcher.
func NewBlocklist(path string, logger *slog.Logger) (*Blocklist, error) {
	bl := &Blocklist{logger: logger}

	// Load hardcoded defaults first.
	defaults := hardcodedDefaults()
	bl.current.Store(defaults)
	bl.healthy = true

	// Attempt to load from file (merges on top of defaults).
	if path != "" {
		if err := bl.loadFromFile(path); err != nil {
			logger.Warn("blocklist file load failed; using hardcoded defaults (fail-closed)",
				"path", path, "error", err)
			// Do NOT return an error — we are fail-closed with defaults active.
			// The daemon can start; it will just use the hardcoded list.
		}
	}

	// Start file watcher for hot-reload.
	if path != "" {
		if err := bl.watchFile(path); err != nil {
			// Non-fatal: hot-reload won't work, but current blocklist is active.
			logger.Warn("blocklist file watcher failed; hot-reload disabled", "error", err)
		}
	}

	return bl, nil
}

// Check evaluates a WindowContext and returns a BlockDecision.
// This is the hot path — must be fast (< 2ms).
//
// CRITICAL: This function determines whether pixels are ever read.
// When in doubt, it returns Blocked=true.
func (bl *Blocklist) Check(ctx WindowContext) BlockDecision {
	compiled := bl.current.Load()
	if compiled == nil {
		// No blocklist loaded at all — fail closed.
		return BlockDecision{Blocked: true, Reason: "blocklist_unavailable"}
	}

	// Check 1: Process name (exact, case-insensitive).
	pname := strings.ToLower(ctx.ProcessName)
	if _, ok := compiled.apps[pname]; ok {
		return BlockDecision{Blocked: true, Reason: fmt.Sprintf("app_match:%s", ctx.ProcessName)}
	}

	// Check 2: Auth process patterns.
	for p := range compiled.authPatterns {
		if strings.Contains(pname, p) {
			return BlockDecision{Blocked: true, Reason: fmt.Sprintf("auth_process:%s", ctx.ProcessName)}
		}
	}

	// Check 3: Browser URL regex (only if URL is available).
	if ctx.BrowserURL != "" {
		for _, re := range compiled.urlRegexps {
			if re.MatchString(ctx.BrowserURL) {
				return BlockDecision{Blocked: true, Reason: fmt.Sprintf("url_pattern:%s", re.String())}
			}
		}
	}

	// Check 4: Window title regex.
	if ctx.WindowTitle != "" {
		for _, re := range compiled.titleRegexps {
			if re.MatchString(ctx.WindowTitle) {
				return BlockDecision{Blocked: true, Reason: fmt.Sprintf("title_pattern:%s", re.String())}
			}
		}
	}

	// Check 5: Sensitive input role (e.g., password field is focused).
	if ctx.FocusedInputRole != "" {
		if _, ok := compiled.sensitiveInputs[strings.ToLower(ctx.FocusedInputRole)]; ok {
			return BlockDecision{Blocked: true, Reason: fmt.Sprintf("sensitive_input:%s", ctx.FocusedInputRole)}
		}
	}

	return BlockDecision{Blocked: false, Reason: "pass"}
}

// Close stops the file watcher goroutine.
func (bl *Blocklist) Close() error {
	if bl.watcher != nil {
		return bl.watcher.Close()
	}
	return nil
}

// loadFromFile reads and compiles the blocklist at path.
// On failure, the existing (default) blocklist remains active.
func (bl *Blocklist) loadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %q: %w", path, err)
	}

	var cfg BlocklistConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing %q: %w", path, err)
	}

	compiled, err := compile(&cfg)
	if err != nil {
		return fmt.Errorf("compiling blocklist from %q: %w", path, err)
	}

	// Merge with hardcoded defaults: file entries are ADDED to defaults, not replacing them.
	merged := mergeWithDefaults(compiled, hardcodedDefaults())

	// Atomic swap: new blocklist becomes active only after full compilation succeeds.
	bl.mu.Lock()
	bl.current.Store(merged)
	bl.healthy = true
	bl.mu.Unlock()

	bl.logger.Info("blocklist reloaded", "path", path,
		"apps", len(merged.apps),
		"url_patterns", len(merged.urlRegexps),
		"title_patterns", len(merged.titleRegexps))

	return nil
}

// watchFile starts a goroutine that reloads the blocklist when the file changes.
func (bl *Blocklist) watchFile(path string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(path); err != nil {
		w.Close()
		return err
	}
	bl.watcher = w

	go func() {
		for {
			select {
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					bl.logger.Info("blocklist file changed; hot-reloading", "path", path)
					if err := bl.loadFromFile(path); err != nil {
						bl.logger.Warn("hot-reload failed; previous blocklist still active", "error", err)
					}
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				bl.logger.Warn("blocklist watcher error", "error", err)
			}
		}
	}()

	return nil
}

// compile converts a BlocklistConfig into a compiledBlocklist.
func compile(cfg *BlocklistConfig) (*compiledBlocklist, error) {
	c := &compiledBlocklist{
		apps:            make(map[string]struct{}),
		authPatterns:    make(map[string]struct{}),
		sensitiveInputs: make(map[string]struct{}),
	}

	for _, app := range cfg.Apps {
		c.apps[strings.ToLower(app)] = struct{}{}
	}
	for _, p := range cfg.AuthPatterns {
		c.authPatterns[strings.ToLower(p)] = struct{}{}
	}
	for _, p := range cfg.SensitiveInputRoles {
		c.sensitiveInputs[strings.ToLower(p)] = struct{}{}
	}

	for _, pattern := range cfg.URLPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid url_pattern %q: %w", pattern, err)
		}
		c.urlRegexps = append(c.urlRegexps, re)
	}

	for _, pattern := range cfg.TitlePatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid title_pattern %q: %w", pattern, err)
		}
		c.titleRegexps = append(c.titleRegexps, re)
	}

	return c, nil
}

// mergeWithDefaults returns a new compiledBlocklist that contains all entries
// from both src and defaults. Neither is modified.
func mergeWithDefaults(src, defaults *compiledBlocklist) *compiledBlocklist {
	merged := &compiledBlocklist{
		apps:            make(map[string]struct{}),
		authPatterns:    make(map[string]struct{}),
		sensitiveInputs: make(map[string]struct{}),
	}
	for k := range defaults.apps {
		merged.apps[k] = struct{}{}
	}
	for k := range src.apps {
		merged.apps[k] = struct{}{}
	}
	for k := range defaults.authPatterns {
		merged.authPatterns[k] = struct{}{}
	}
	for k := range src.authPatterns {
		merged.authPatterns[k] = struct{}{}
	}
	for k := range defaults.sensitiveInputs {
		merged.sensitiveInputs[k] = struct{}{}
	}
	for k := range src.sensitiveInputs {
		merged.sensitiveInputs[k] = struct{}{}
	}
	merged.urlRegexps = append(defaults.urlRegexps, src.urlRegexps...)
	merged.titleRegexps = append(defaults.titleRegexps, src.titleRegexps...)
	return merged
}

// hardcodedDefaults returns the minimum blocklist that is always active,
// regardless of whether a config file is present or parseable.
// These are non-negotiable privacy protections.
func hardcodedDefaults() *compiledBlocklist {
	apps := []string{
		"1password", "1password 7", "1password 8",
		"bitwarden",
		"keepass", "keepassxc",
		"dashlane",
		"lastpass",
		"keychain access",
		"gnome-keyring-daemon",
		"kwallet", "kwalletmanager",
		"pinentry", "pinentry-gtk", "pinentry-qt", "pinentry-curses", "pinentry-gnome3",
		"ssh-askpass",
		"seahorse",
	}
	authProcs := []string{
		"sudo", "polkit", "pkttyagent", "pkexec",
		"consent.exe",
	}
	urlPatterns := []string{
		// Digital Ghost's own search UI — capturing it is circular and useless.
		// This is a hard system invariant baked into defaults (not user-editable).
		`(?i)^https?://(127\.0\.0\.1|localhost):7327(/|$)`,
		`(?i)bank`, `(?i)banking`,
		`(?i)paypal\.com`, `(?i)stripe\.com/dashboard`,
		`(?i)\.irs\.gov`, `(?i)turbotax\.com`,
		`(?i)mychart\.com`, `(?i)epic\.com`,
		`(?i)/login`, `(?i)/signin`, `(?i)/auth`,
		`(?i)/password`, `(?i)/2fa`, `(?i)/mfa`,
		`(?i)/reset-password`, `(?i)/forgot-password`,
		`(?i)mail\.google\.com`, `(?i)outlook\.live\.com`,
		`(?i)proton\.me`, `(?i)protonmail\.com`,
	}
	titlePatterns := []string{
		// Digital Ghost self-capture: title fallback for when Firefox URL extraction
		// returns empty (UIA fails silently). URL pattern alone is insufficient.
		`(?i)digital ghost`,
		`(?i)password`, `(?i)passphrase`, `(?i)secret`,
		`(?i)private key`, `(?i)login`, `(?i)sign in`,
		`(?i)authenticate`, `(?i)\.env`, `(?i)credentials`,
		`(?i)api.?key`, `(?i)token`,
	}
	sensitiveInputs := []string{"password", "spinbutton"}

	cfg := &BlocklistConfig{
		Apps:               apps,
		AuthPatterns:       authProcs,
		URLPatterns:        urlPatterns,
		TitlePatterns:      titlePatterns,
		SensitiveInputRoles: sensitiveInputs,
	}

	// compile() can only fail on invalid regex; all patterns above are valid.
	compiled, err := compile(cfg)
	if err != nil {
		// This is a programming error, not a runtime error.
		panic(fmt.Sprintf("hardcoded blocklist has invalid regex: %v", err))
	}
	return compiled
}

// atomicPointerSize ensures the atomic.Pointer is correctly sized for unsafe operations.
var _ = unsafe.Sizeof(atomic.Pointer[compiledBlocklist]{})
