// cmd/agents/main.go — Digital Ghost Agent Team
//
// Three Claude-powered agents collaborate to generate and gatekeep new
// Digital Ghost features in an iterative review loop:
//
//	💡 Ideas Agent   — proposes new features, refines based on feedback
//	🏗️  Architect     — reviews for architecture fit and technical feasibility
//	🔒 CISO          — reviews for security, privacy, and threat model
//
// Flow:
//
//	Ideas proposes → Arch + CISO review in parallel → Ideas refines
//	→ repeat for --rounds cycles → final sign-off from Arch + CISO
//
// Usage:
//
//	ANTHROPIC_API_KEY=sk-ant-... go run ./cmd/agents [flags]
//	  -rounds int    review+refine cycles (default 2)
//	  -topic  string optional focus area  (e.g. "timeline UX", "performance")
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// All agents run on Opus 4.6 with adaptive thinking — the most capable model
// for deep multi-domain reasoning. Adaptive thinking lets each agent decide how
// much internal deliberation the task warrants.
const agentModel = anthropic.ModelClaudeOpus4_6

// ── System prompts ──────────────────────────────────────────────────────────

const ideasSystemPrompt = `You are the Digital Ghost Ideas Agent — the creative engine of the DG development team.

ABOUT DIGITAL GHOST
Digital Ghost (DG) is a local-only, air-gapped, privacy-first screen memory tool:
  • Captures the screen at 2 FPS using GDI/DXGI (Windows) or X11 (Linux)
  • Privacy gate checks foreground window metadata BEFORE any pixel is ever read (fail-closed)
  • Local VLM inference: Ollama llava:7b describes each unique frame on-device
  • Storage: AES-256-GCM encrypted memory nodes in a flat-file JSON vector store
  • Encryption key lives ONLY in the OS keychain (DPAPI on Windows) — never touches disk
  • Similarity search via nomic-embed-text embeddings, all local
  • Search + chat UI: single-page app at http://localhost:7327 (loopback only)
  • System tray icon (mandatory — no silent background mode)
  • Zero network egress: no cloud APIs, no telemetry, nothing leaves the machine

NON-NEGOTIABLE INVARIANTS (the Architect and CISO will reject any feature that breaks these)
  1. Privacy gate is FIRST — window metadata checked before any pixel is read, always
  2. Fail-closed — missing or corrupt blocklist halts all capture entirely
  3. Resource budget is hard — CPU/GPU governor cannot be bypassed
  4. Key never on disk — encryption key lives only in OS keychain + process memory
  5. Consent required — daemon refuses to start without a valid consent record
  6. Tray icon mandatory — no silent background mode
  7. Zero network egress — no cloud, no telemetry, nothing leaves the machine

YOUR ROLE
Propose bold, user-valuable new features for Digital Ghost. For each feature:
  • Name            — memorable 2–4 word name
  • User Story      — "As a DG user, I want to… so that…"
  • Philosophy Fit  — how it honours the local-only, privacy-first, zero-egress design
  • Implementation  — which internal packages change, rough approach in plain Go/Ollama terms
  • Value           — why users will love this; what problem does it solve?

When you receive Architect and CISO feedback:
  • Features marked ✅ by both reviewers — confirm them, add implementation detail
  • Features marked ⚠️ CONDITIONAL — incorporate every required change
  • Features marked ❌ by either reviewer — redesign to address the blocker, or replace with a fresh idea
  • Be honest: if a rejected feature has an unfixable conflict with the invariants, drop it and explain why`

const archSystemPrompt = `You are the Digital Ghost Architect — the technical guardian of DG's design.

SYSTEM ARCHITECTURE
  cmd/digitalghost/main.go    entry point; wires capture → inference → storage → API → tray
  internal/capture/           GDI/DXGI pixel capture, X11 fallback, privacy gate, pHash dedup
  internal/filter/            fail-closed blocklist (hot-reload), content classifier, engagement scorer
  internal/inference/         Ollama HTTP client, bounded frame queue, resource governor / throttle
  internal/storage/           AES-256-GCM per-record encryption, flat-file JSON vector store,
                              DPAPI keychain, retention sweep + secure deletion
  internal/api/               HTTP server (127.0.0.1:7327 only), embedded search+chat UI,
                              CSRF protection on mutating endpoints
  internal/tray/              consent manager, Windows MessageBoxW, systray menu

NON-NEGOTIABLE INVARIANTS (you enforce these — any feature that breaks them is ❌ NO-GO)
  1. Privacy gate is FIRST — window metadata before pixels, without exception
  2. Fail-closed — missing/corrupt blocklist halts capture entirely
  3. Resource budget is hard — no bypass of the CPU/GPU governor
  4. Key never on disk
  5. Consent required to start
  6. Tray icon mandatory

YOUR ROLE
For each proposed feature, assess:
  • Feasibility    — can this be built with Go + Ollama + local Win32/X11 APIs? what dependencies?
  • Fit            — which invariants are touched? how does it integrate with existing packages?
  • Complexity     — implementation effort: S (days) | M (1–2 weeks) | L (1 month) | XL (quarter)
  • Impact         — what existing components change significantly?
  • Concerns       — specific technical blockers or risks; suggest alternative approaches

Verdict per feature — be decisive:
  ✅ GO                — approve as proposed
  ⚠️ CONDITIONAL       — approve if the listed requirements are met (be specific)
  ❌ NO-GO             — reject; explain the blocker clearly so Ideas can redesign`

const cisoSystemPrompt = `You are the Digital Ghost CISO — guardian of DG's security, privacy, and threat model.

THREAT MODEL (what DG defends against)
  T1 — Malicious local processes reading DG's memory store
  T2 — Network exfiltration of screen data or memories
  T3 — Physical access attacks on the machine
  T4 — Supply chain attacks via malicious dependencies
  T5 — Self-capture feedback loops (DG capturing its own UI → VLM hallucination spiral)
  T6 — Privilege escalation or abuse of DG's screen-capture capabilities

EXISTING SECURITY CONTROLS
  • AES-256-GCM per-record encryption; key in OS keychain (DPAPI), never on disk
  • Zero network egress — no cloud calls, no telemetry; HTTP server binds 127.0.0.1 only
  • Fail-closed blocklist — missing = all capture halted; corrupt = all capture halted
  • Consent required before any capture begins; shown again if consent text changes
  • Secure deletion with daily retention sweeps (2 am)
  • Privacy gate checked BEFORE any pixel is read
  • CSRF token on all mutating API endpoints
  • Loopback-only HTTP server (never reachable from the network)
  • Sensitive input masking (password fields blacked out in captured frames)

YOUR ROLE
For each proposed feature, evaluate:
  • Attack surface   — new trust boundaries, IPC channels, sockets, or file paths?
  • Privacy regression — does this weaken any existing privacy guarantee?
  • Exfiltration risk  — could this create a path for memories to leave the device?
  • New secrets        — new credentials, tokens, or auth flows introduced?
  • Abuse potential    — could a malicious local process or website exploit this?
  • Required mitigations — specific controls that MUST be implemented (not nice-to-haves)

Verdict per feature — be decisive:
  ✅ APPROVE                      — approve as proposed; no additional controls needed
  ⚠️ APPROVE WITH CONDITIONS      — approve only if every listed mitigation is implemented
  ❌ REJECT                       — fundamental conflict with threat model or invariants; explain clearly`

// ── Agent ───────────────────────────────────────────────────────────────────

// Agent is a stateful Claude agent. Each agent keeps its own conversation
// history so context accumulates across the review loop.
type Agent struct {
	name    string
	emoji   string
	system  string
	history []anthropic.MessageParam
	client  *anthropic.Client
}

func newAgent(name, emoji, system string, client *anthropic.Client) *Agent {
	return &Agent{name: name, emoji: emoji, system: system, client: client}
}

// chat sends userMsg to the agent and returns the full response text.
// When verbose=true, the response is streamed to stdout as it arrives.
// Conversation history is preserved for multi-turn context.
func (a *Agent) chat(ctx context.Context, userMsg string, verbose bool) (string, error) {
	a.history = append(a.history, anthropic.NewUserMessage(anthropic.NewTextBlock(userMsg)))

	// Adaptive thinking: each agent decides how much internal reasoning the
	// task needs. No budget_tokens needed — the model manages its own budget.
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}

	stream := a.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     agentModel,
		MaxTokens: 8192,
		System:    []anthropic.TextBlockParam{{Text: a.system}},
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		Messages:  a.history,
	})

	msg := anthropic.Message{}
	var sb strings.Builder

	for stream.Next() {
		event := stream.Current()
		// Accumulate every event so msg.ToParam() captures thinking + text blocks
		// for the next turn. Omitting thinking blocks from history breaks multi-turn.
		msg.Accumulate(event)

		if de, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if td, ok := de.Delta.AsAny().(anthropic.TextDelta); ok {
				sb.WriteString(td.Text)
				if verbose {
					fmt.Print(td.Text)
				}
			}
		}
	}

	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("[%s] stream: %w", a.name, err)
	}
	if verbose {
		fmt.Println()
	}

	// Preserve the full assistant turn (including thinking blocks) in history.
	a.history = append(a.history, msg.ToParam())
	return sb.String(), nil
}

// ── Parallel review ─────────────────────────────────────────────────────────

type reviewResult struct {
	name  string
	emoji string
	text  string
	err   error
}

// reviewParallel sends proposals to Architect and CISO concurrently.
// Both agents review quietly (no stdout interleaving); results are printed
// sequentially — Architect first, then CISO — once both finish.
func reviewParallel(ctx context.Context, arch, ciso *Agent, proposals string) (archText, cisoText string, err error) {
	ch := make(chan reviewResult, 2)
	prompt := "Please review the following feature proposals:\n\n" + proposals

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		t, e := arch.chat(ctx, prompt, false)
		ch <- reviewResult{arch.name, arch.emoji, t, e}
	}()
	go func() {
		defer wg.Done()
		t, e := ciso.chat(ctx, prompt, false)
		ch <- reviewResult{ciso.name, ciso.emoji, t, e}
	}()

	wg.Wait()
	close(ch)

	results := make(map[string]reviewResult, 2)
	for r := range ch {
		if r.err != nil {
			return "", "", fmt.Errorf("[%s] review failed: %w", r.name, r.err)
		}
		results[r.name] = r
	}

	// Print in deterministic order: Architect → CISO.
	for _, agent := range []struct{ name, emoji string }{{arch.name, arch.emoji}, {ciso.name, ciso.emoji}} {
		r := results[agent.name]
		agentHeader(r.emoji, r.name)
		fmt.Println(r.text)
	}

	return results[arch.name].text, results[ciso.name].text, nil
}

// ── Orchestration loop ───────────────────────────────────────────────────────

func run(ctx context.Context, rounds int, topic string) error {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY environment variable is not set\n" +
			"  Set it with: $env:ANTHROPIC_API_KEY = 'sk-ant-...'  (PowerShell)\n" +
			"           or: export ANTHROPIC_API_KEY=sk-ant-...     (bash/zsh)")
	}

	c := anthropic.NewClient(option.WithAPIKey(apiKey))

	ideas := newAgent("Ideas Agent", "💡", ideasSystemPrompt, &c)
	arch := newAgent("Architect", "🏗️ ", archSystemPrompt, &c)
	ciso := newAgent("CISO", "🔒", cisoSystemPrompt, &c)

	printBanner(rounds, topic)

	// ── Step 1: Initial proposals ──────────────────────────────────────────
	seed := "Propose 3 new features for Digital Ghost. " +
		"For each: Name, User Story, Philosophy Fit, Implementation Sketch, Value Proposition."
	if topic != "" {
		seed = fmt.Sprintf(
			"Propose 3 new features for Digital Ghost focused on %q. "+
				"For each: Name, User Story, Philosophy Fit, Implementation Sketch, Value Proposition.",
			topic)
	}

	agentHeader(ideas.emoji, ideas.name+" — Initial Proposals")
	proposal, err := ideas.chat(ctx, seed, true)
	if err != nil {
		return err
	}

	// ── Steps 2–N: Review + refine cycles ─────────────────────────────────
	for round := 1; round <= rounds; round++ {
		printRoundBanner(round, rounds)

		fmt.Printf("  %s Architect and %s CISO reviewing in parallel...\n\n", arch.emoji, ciso.emoji)
		archFeedback, cisoFeedback, err := reviewParallel(ctx, arch, ciso, proposal)
		if err != nil {
			return err
		}

		refinePrompt := fmt.Sprintf(
			"Here is the feedback from your reviewers.\n\n"+
				"══════════════ ARCHITECT FEEDBACK ══════════════\n%s\n\n"+
				"══════════════ CISO FEEDBACK ══════════════\n%s\n\n"+
				"Please revise your proposals:\n"+
				"  • ✅ features approved by both — confirm them and add any missing implementation detail\n"+
				"  • ⚠️ CONDITIONAL features — incorporate every required change the reviewer listed\n"+
				"  • ❌ features rejected by either — redesign to address the specific blocker, or replace "+
				"with a new idea that fits both the architecture and the threat model\n"+
				"Output the complete revised feature set.",
			archFeedback, cisoFeedback,
		)

		agentHeader(ideas.emoji, fmt.Sprintf("%s — Round %d Revision", ideas.name, round))
		proposal, err = ideas.chat(ctx, refinePrompt, true)
		if err != nil {
			return err
		}
	}

	// ── Final sign-off ─────────────────────────────────────────────────────
	printSignoffBanner()

	fmt.Printf("  %s Architect and %s CISO giving final verdicts in parallel...\n\n", arch.emoji, ciso.emoji)
	_, _, err = reviewParallel(ctx, arch, ciso,
		"These are the final refined proposals. "+
			"Please give your official verdict on each feature: APPROVED or REJECTED, "+
			"with a one-sentence rationale. This record goes into the implementation backlog.")
	return err
}

func main() {
	rounds := flag.Int("rounds", 2, "number of review+refine cycles (default 2)")
	topic := flag.String("topic", "", "optional feature focus area (e.g. \"search UX\", \"performance\")")
	flag.Parse()

	// 25 minutes: 3 features × (Ideas + Arch + CISO) × 2 rounds + sign-off
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	if err := run(ctx, *rounds, *topic); err != nil {
		fmt.Fprintf(os.Stderr, "\n\nerror: %v\n", err)
		os.Exit(1)
	}
}

// ── Display helpers ──────────────────────────────────────────────────────────

func printBanner(rounds int, topic string) {
	fmt.Println()
	fmt.Println(strings.Repeat("═", 72))
	fmt.Printf("  ✦  Digital Ghost Agent Team  ✦  %d review round(s)\n", rounds)
	if topic != "" {
		fmt.Printf("  Focus area: %s\n", topic)
	}
	fmt.Println(strings.Repeat("═", 72))
}

func printRoundBanner(round, total int) {
	fmt.Println()
	fmt.Println(strings.Repeat("─", 72))
	fmt.Printf("  ROUND %d / %d — Architect + CISO reviewing in parallel\n", round, total)
	fmt.Println(strings.Repeat("─", 72))
}

func printSignoffBanner() {
	fmt.Println()
	fmt.Println(strings.Repeat("═", 72))
	fmt.Println("  FINAL SIGN-OFF — Official backlog verdicts")
	fmt.Println(strings.Repeat("═", 72))
}

func agentHeader(emoji, name string) {
	fmt.Printf("\n── %s  %s ──────────────────────────────────────────\n\n", emoji, name)
}
