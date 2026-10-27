// Package meter turns a Claude response's token usage into two comparable
// numbers: weighted tokens (every model normalized onto one scale so a person's
// contribution is comparable across models) and USD cost. It is the piece the
// retired usage-monitor got wrong in two specific ways, both fixed here:
//
//   - It counted only input+output, ignoring cache tokens. We count all four
//     (input, output, cache-creation, cache-read), each at its own rate.
//   - It guessed an unknown model's weight by family substring, which once
//     overcharged the whole fleet 50%. Here an unmatched model is reported as
//     unknown (Known=false) and never silently inherits another model's rate.
package meter

import "strings"

// Usage is the token breakdown from a response's `message.usage`.
type Usage struct {
	Input         int64
	Output        int64
	CacheCreation int64 // cache_creation_input_tokens (written to cache)
	CacheRead     int64 // cache_read_input_tokens (served from cache)
}

// rate is one model's pricing, in USD per million tokens. Weight is the same
// pricing normalized so a Sonnet-4.x input token is 1.0 — the scale the boards
// compare people on, independent of the dollar amounts.
type rate struct {
	inUSD, outUSD float64 // per 1M tokens
	weight        float64 // normalized to sonnet-4.x input = 1.0
}

// baselineInputUSD is Sonnet-4.x's input price; weight = inUSD / baseline.
const baselineInputUSD = 3.0

// cacheCreationMult / cacheReadMult follow Anthropic's cache pricing: writing to
// the cache costs 1.25x the input rate, reading from it 0.1x.
const (
	cacheCreationMult = 1.25
	cacheReadMult     = 0.10
	outputMult        = 5.0 // output is ~5x input across the line ($15 vs $3, etc.)
)

// rates maps a model family (matched by the longest ID prefix below) to its
// pricing. New models are added here explicitly — that is the whole point.
var rates = []struct {
	prefix string
	rate   rate
}{
	// Mythos-class (Fable 5.1 / Mythos 5.1): the top tier, ~3.33x Sonnet input.
	{"claude-fable-5", rate{inUSD: 10, outUSD: 50, weight: 10.0 / baselineInputUSD}},
	{"claude-mythos-5", rate{inUSD: 10, outUSD: 50, weight: 10.0 / baselineInputUSD}},
	// Opus 5 / 4.x: ~1.667x Sonnet input.
	{"claude-opus", rate{inUSD: 5, outUSD: 25, weight: 5.0 / baselineInputUSD}},
	// Sonnet 5: cheaper than 4.x, ~0.667x.
	{"claude-sonnet-5", rate{inUSD: 2, outUSD: 10, weight: 2.0 / baselineInputUSD}},
	// Sonnet 4.x: the 1.0 baseline.
	{"claude-sonnet-4", rate{inUSD: 3, outUSD: 15, weight: 1.0}},
	{"claude-sonnet-3", rate{inUSD: 3, outUSD: 15, weight: 1.0}},
	// Haiku: ~0.333x.
	{"claude-haiku", rate{inUSD: 1, outUSD: 5, weight: 1.0 / baselineInputUSD}},
}

// Rate returns a model's pricing. known is false for a model this build has
// never heard of — callers record it as model "unknown" and surface it, rather
// than pretend it costs the same as something else.
func rateFor(model string) (rate, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	// Longest-prefix wins: rates is ordered specific-before-general (sonnet-5
	// before sonnet-4 before... none overlap, but the explicit order documents
	// intent and guards a future "claude-sonnet-5-mini").
	for _, r := range rates {
		if strings.HasPrefix(m, r.prefix) {
			return r.rate, true
		}
	}
	return rate{}, false
}

// Measure turns a model + usage into weighted tokens and USD cost. When the
// model is unknown, Known is false and both numbers are 0 — the caller stores
// the raw token counts under model "unknown" so nothing is lost and the gap is
// visible, but it is never attributed a cost or weight it might not have.
type Measured struct {
	Weighted float64
	CostUSD  float64
	Known    bool
}

func Measure(model string, u Usage) Measured {
	r, ok := rateFor(model)
	if !ok {
		return Measured{Known: false}
	}
	weighted := partsAt(r, u).Sum()

	// USD: input priced at inUSD/MTok, the rest scaled off it by the same
	// multipliers (outUSD is carried explicitly and used for output).
	const perMillion = 1_000_000.0
	cost := float64(u.Input)/perMillion*r.inUSD +
		float64(u.Output)/perMillion*r.outUSD +
		float64(u.CacheCreation)/perMillion*r.inUSD*cacheCreationMult +
		float64(u.CacheRead)/perMillion*r.inUSD*cacheReadMult

	return Measured{Weighted: weighted, CostUSD: cost, Known: true}
}

// Parts is a usage's weighted tokens taken class by class; they add up to what
// Measure calls Weighted.
type Parts struct {
	Input, Output, CacheCreation, CacheRead float64
}

// Sum is the parts added up.
func (p Parts) Sum() float64 { return p.Input + p.Output + p.CacheCreation + p.CacheRead }

// WeightedParts is Measure's weighted tokens split by token class, so a board can
// show which kind of token a figure came from. An unknown model's parts are all
// zero, as its weight is.
func WeightedParts(model string, u Usage) Parts {
	r, ok := rateFor(model)
	if !ok {
		return Parts{}
	}
	return partsAt(r, u)
}

// partsAt scales every token class to the input-token baseline for the model,
// then applies the model's weight. Output and cache classes use the same
// multipliers the dollar cost does, so weight and cost stay in step.
func partsAt(r rate, u Usage) Parts {
	return Parts{
		Input:         float64(u.Input) * r.weight,
		Output:        float64(u.Output) * outputMult * r.weight,
		CacheCreation: float64(u.CacheCreation) * cacheCreationMult * r.weight,
		CacheRead:     float64(u.CacheRead) * cacheReadMult * r.weight,
	}
}
