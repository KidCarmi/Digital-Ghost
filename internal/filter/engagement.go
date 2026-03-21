// engagement.go computes an engagement score for each VLM inference result.
//
// The engagement score is the primary mechanism for preventing semantic pollution:
// frames the user glanced at briefly are scored low and discarded; content the
// user actively interacted with is scored high and stored.
//
// Score formula:
//
//	engagement_score = dwell_weight × interaction_weight × content_class_weight
//
// Scores are in the range [0.0, 1.0+].
// Frames with score below MinEngagementScore (default: 0.3) are discarded.
package filter

import (
	"math"
	"time"
)

// EngagementSignals contains all observable signals about the user's interaction
// with a piece of content, collected from the OS input subsystem.
type EngagementSignals struct {
	// DwellSeconds is how long the window was in the foreground before this frame
	// was captured. Longer dwell = higher engagement.
	DwellSeconds float64

	// TypedWithinSeconds: if the user typed within this many seconds of capture,
	// the value is positive. 0 means no recent typing.
	TypedWithinSeconds float64

	// ScrolledWithinSeconds: if the user scrolled within this many seconds. 0 = no.
	ScrolledWithinSeconds float64

	// ClickedWithinSeconds: if the user clicked within this many seconds. 0 = no.
	ClickedWithinSeconds float64

	// ContentClass is the category assigned by the fast content classifier.
	ContentClass ContentClass

	// ViewCount is the number of times this approximate content has been seen
	// (via pHash clustering). Higher view count = the user returned to this content.
	ViewCount int

	// CapturedAt is the timestamp of the frame (for time-of-day weighting).
	CapturedAt time.Time
}

// ContentClass categorizes content for semantic weight.
type ContentClass int

const (
	ClassUnknown      ContentClass = iota
	ClassWorkDocument              // Google Docs, Office, Notion, PDFs
	ClassWorkCode                  // VS Code, terminals, GitHub
	ClassWorkResearch              // Tech articles, papers, Stack Overflow
	ClassCommunication             // Slack, email, Jira
	ClassSystemUI                  // File manager, Settings
	ClassEntertainment             // YouTube, Netflix, gaming
	ClassSocial                    // Twitter, Reddit (general)
)

// classWeights maps content classes to their base engagement weight.
// Entertainment and system UI are heavily penalized; work content is neutral to high.
var classWeights = map[ContentClass]float64{
	ClassUnknown:       0.5,
	ClassWorkDocument:  1.0,
	ClassWorkCode:      1.0,
	ClassWorkResearch:  0.9,
	ClassCommunication: 0.7,
	ClassSystemUI:      0.2,
	ClassEntertainment: 0.1,
	ClassSocial:        0.15,
}

// EngagementScore computes the engagement score for a given set of signals.
// Returns a value in [0.0, ∞); typical range is [0.0, 2.0].
// Frames with score < cfg.MinEngagementScore should be discarded.
func EngagementScore(signals EngagementSignals) float64 {
	dwellW := dwellWeight(signals.DwellSeconds)
	interactionW := interactionWeight(signals)
	contentW := contentClassWeight(signals.ContentClass)
	viewW := viewCountBoost(signals.ViewCount)

	score := dwellW * interactionW * contentW * viewW
	return score
}

// dwellWeight converts dwell time to a weight in [0.1, 1.0].
// < 1s: 0.1 (flash-visible, likely a tooltip or transient)
// 3s: 0.4 (minimum meaningful dwell)
// 10s: 0.7
// 30s+: 1.0 (user is clearly reading this)
func dwellWeight(seconds float64) float64 {
	if seconds < 1 {
		return 0.1
	}
	// Logarithmic growth: reaches 1.0 at ~30 seconds.
	w := math.Log1p(seconds) / math.Log1p(30)
	if w > 1.0 {
		return 1.0
	}
	if w < 0.1 {
		return 0.1
	}
	return w
}

// interactionWeight returns a multiplier based on recent user interactions.
// Typing is the strongest signal (user is actively engaging with content);
// passive viewing is the weakest.
func interactionWeight(s EngagementSignals) float64 {
	// Use the strongest interaction signal present within 10 seconds.
	const recentThreshold = 10.0

	if s.TypedWithinSeconds > 0 && s.TypedWithinSeconds <= recentThreshold {
		return 3.0 // Typing = highest engagement
	}
	if s.ScrolledWithinSeconds > 0 && s.ScrolledWithinSeconds <= recentThreshold {
		return 2.0 // Scrolling = reading/navigating
	}
	if s.ClickedWithinSeconds > 0 && s.ClickedWithinSeconds <= recentThreshold {
		return 1.5 // Click = navigating
	}
	return 1.0 // Passive viewing
}

// contentClassWeight returns the semantic weight for a content class.
func contentClassWeight(class ContentClass) float64 {
	if w, ok := classWeights[class]; ok {
		return w
	}
	return classWeights[ClassUnknown]
}

// viewCountBoost returns a logarithmic boost for content seen multiple times.
// Seeing the same content 3 times suggests the user is actively working with it.
// The boost is capped to prevent infinite accumulation.
func viewCountBoost(viewCount int) float64 {
	if viewCount <= 1 {
		return 1.0
	}
	// log(1) = 0, log(2) ≈ 0.69, log(10) ≈ 2.30. Cap at 2.0.
	boost := 1.0 + math.Log(float64(viewCount))
	if boost > 2.0 {
		return 2.0
	}
	return boost
}
