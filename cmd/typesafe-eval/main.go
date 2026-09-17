// Command typesafe-eval evaluates a labeled JSONL corpus without delivering mail.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/spam"
)

type corpusEntry struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	// These must be trusted server results exported alongside the corpus.
	AuthResults map[string]any `json:"auth_results,omitempty"`
}

type prediction struct {
	Path              string                 `json:"path"`
	Label             string                 `json:"label"`
	RequestedModel    string                 `json:"requested_model"`
	Model             string                 `json:"model,omitempty"`
	Status            string                 `json:"status"`
	QuestionVersion   string                 `json:"question_version"`
	ExtractorVersion  string                 `json:"extractor_version"`
	Spam              *float64               `json:"spam_probability"`
	Phishing          *float64               `json:"phishing_probability"`
	SpamThreshold     float64                `json:"spam_threshold"`
	PhishingThreshold float64                `json:"phishing_threshold"`
	WouldJunk         *bool                  `json:"would_junk"`
	ElapsedMS         int64                  `json:"elapsed_ms"`
	Input             spam.TypeSafeInputInfo `json:"input"`
	Usage             *spam.TypeSafeUsage    `json:"usage"`
}

type summary struct {
	Total                int      `json:"total"`
	Evaluated            int      `json:"evaluated"`
	Unscored             int      `json:"unscored"`
	Ham                  int      `json:"evaluated_ham"`
	Unwanted             int      `json:"evaluated_unwanted"`
	FalsePositives       int      `json:"false_positives"`
	TruePositives        int      `json:"true_positives"`
	PredictedJunk        int      `json:"predicted_junk"`
	HamFalsePositiveRate *float64 `json:"ham_false_positive_rate"`
	Precision            *float64 `json:"precision"`
	Recall               *float64 `json:"recall"`
	FallbackRate         *float64 `json:"fallback_rate"`
	LatencyP50MS         *int64   `json:"latency_p50_ms"`
	LatencyP95MS         *int64   `json:"latency_p95_ms"`
	InputTokens          int64    `json:"input_tokens"`
	OutputTokens         int64    `json:"output_tokens"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Getenv("HAITATSU_TYPESAFE_API_KEY"), os.Stdout, os.Stderr, nil, wait); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, key string, out, diagnostics io.Writer, evaluator spam.TypeSafeEvaluator, pause func(context.Context, time.Duration) error) error {
	fs := flag.NewFlagSet("typesafe-eval", flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	corpus := fs.String("corpus", "", "JSONL manifest with path and ham/spam/phishing label (required)")
	model := fs.String("model", "jev-latest", "TypeSafe model")
	timeout := fs.Int("timeout-ms", 2000, "per-request timeout, 100–10000 ms")
	maxText := fs.Int("max-text-bytes", 16384, "combined text budget, up to 65536 bytes")
	rpm := fs.Int("max-requests-per-minute", 60, "per-process request budget; requests are paced")
	spamThreshold := fs.Float64("spam-threshold", .98, "Junk probability threshold for spam")
	phishingThreshold := fs.Float64("phishing-threshold", .98, "Junk probability threshold for phishing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *corpus == "" || fs.NArg() != 0 {
		return errors.New("an explicit -corpus JSONL manifest is required")
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("HAITATSU_TYPESAFE_API_KEY is required")
	}
	if strings.TrimSpace(*model) == "" || *timeout < 100 || *timeout > 10000 || *maxText <= 0 || *maxText > 65536 || *rpm <= 0 || *rpm > 60000 || !validThreshold(*spamThreshold) || !validThreshold(*phishingThreshold) {
		return errors.New("invalid model, timeout, text budget, request budget, or probability threshold")
	}
	entries, err := loadCorpus(*corpus)
	if err != nil {
		return err
	}
	cfg := config.TypeSafeConfig{Mode: "shadow", APIKey: key, Model: *model, TimeoutMS: *timeout, MaxTextBytes: *maxText, MaxInFlight: 1, MaxRequestsPerMinute: *rpm, SpamThreshold: *spamThreshold, PhishingThreshold: *phishingThreshold}
	if evaluator == nil {
		evaluator = spam.NewTypeSafeClient()
	}
	encoder := json.NewEncoder(out)
	totals := summary{Total: len(entries)}
	var latencies []int64
	var next time.Time
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := prediction{Path: entry.Path, Label: entry.Label, RequestedModel: cfg.Model, Status: "error_read", QuestionVersion: spam.TypeSafeQuestionVersion, ExtractorVersion: spam.TypeSafeExtractorVersion, SpamThreshold: cfg.SpamThreshold, PhishingThreshold: cfg.PhishingThreshold}
		path := entry.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(*corpus), path)
		}
		raw, err := readEmail(path)
		if err == nil {
			state, info := spam.BuildTypeSafeState(raw, cfg.MaxTextBytes, entry.AuthResults)
			item.Input, item.Status = info, info.Status
			if info.Status == "ok" || info.Status == "partial" {
				if delay := time.Until(next); delay > 0 {
					if err := pause(ctx, delay); err != nil {
						return err
					}
				}
				evaluation := evaluator.Evaluate(ctx, cfg, state)
				next = time.Now().Add(time.Minute / time.Duration(*rpm))
				item.Status, item.Model, item.ElapsedMS, item.Usage = evaluation.Status, evaluation.Model, evaluation.ElapsedMS, evaluation.Usage
				latencies = append(latencies, evaluation.ElapsedMS)
				if item.Status == "ok" {
					if !validProbability(evaluation.SpamProbability) || !validProbability(evaluation.PhishingProbability) {
						item.Status = "error_invalid_response"
					} else {
						item.Spam, item.Phishing = evaluation.SpamProbability, evaluation.PhishingProbability
						junk := *item.Spam >= cfg.SpamThreshold || *item.Phishing >= cfg.PhishingThreshold
						item.WouldJunk = &junk
					}
				}
			}
		}
		totals.add(item)
		if err := encoder.Encode(item); err != nil {
			return fmt.Errorf("write predictions: %w", err)
		}
	}
	totals.finish(latencies)
	if err := json.NewEncoder(diagnostics).Encode(totals); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

func loadCorpus(path string) ([]corpusEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open corpus: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var entries []corpusEntry
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var entry corpusEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Path == "" || (entry.Label != "ham" && entry.Label != "spam" && entry.Label != "phishing") {
			return nil, fmt.Errorf("invalid corpus entry on line %d: require path and ham/spam/phishing label", line)
		}
		entries = append(entries, entry)
		if len(entries) > 100000 {
			return nil, errors.New("corpus exceeds 100000 entries")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read corpus: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("corpus is empty")
	}
	return entries, nil
}

func readEmail(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// The extractor scans at most 2 MiB. Keep one extra byte to report truncation.
	return io.ReadAll(io.LimitReader(file, 2*1024*1024+1))
}

func (s *summary) add(p prediction) {
	if p.Usage != nil {
		s.InputTokens += int64(p.Usage.InputTokens)
		s.OutputTokens += int64(p.Usage.OutputTokens)
	}
	if p.WouldJunk == nil {
		s.Unscored++
		return
	}
	s.Evaluated++
	if p.Label == "ham" {
		s.Ham++
	} else {
		s.Unwanted++
	}
	if *p.WouldJunk {
		s.PredictedJunk++
		if p.Label == "ham" {
			s.FalsePositives++
		} else {
			s.TruePositives++
		}
	}
}
func (s *summary) finish(latencies []int64) {
	s.HamFalsePositiveRate = ratio(s.FalsePositives, s.Ham)
	s.Precision = ratio(s.TruePositives, s.PredictedJunk)
	s.Recall = ratio(s.TruePositives, s.Unwanted)
	s.FallbackRate = ratio(s.Unscored, s.Total)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		p50, p95 := latencies[(len(latencies)-1)/2], latencies[int(math.Ceil(.95*float64(len(latencies))))-1]
		s.LatencyP50MS, s.LatencyP95MS = &p50, &p95
	}
}
func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	v := float64(n) / float64(d)
	return &v
}
func validThreshold(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 && v <= 1 }
func validProbability(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0 && *v <= 1
}
func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
