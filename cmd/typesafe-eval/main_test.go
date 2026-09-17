package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zephyraoss/haitatsu/internal/config"
	"github.com/zephyraoss/haitatsu/internal/spam"
)

type fakeEvaluator struct{ calls int }

func (f *fakeEvaluator) Evaluate(_ context.Context, _ config.TypeSafeConfig, _ map[string]any) spam.TypeSafeEvaluation {
	f.calls++
	if f.calls == 3 {
		return spam.TypeSafeEvaluation{Status: "error_timeout", ElapsedMS: 2000}
	}
	p := 0.0
	if f.calls == 2 {
		p = 1
	}
	return spam.TypeSafeEvaluation{Status: "ok", Model: "test", SpamProbability: &p, PhishingProbability: new(float64), ElapsedMS: 12, Usage: &spam.TypeSafeUsage{InputTokens: 100, OutputTokens: 10}}
}
func TestLabeledCorpusExcludesFailedPredictions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mail.eml"), []byte("Subject: example\r\n\r\nA message body"), 0600); err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(dir, "labels.jsonl")
	manifest := "{\"path\":\"mail.eml\",\"label\":\"ham\"}\n{\"path\":\"mail.eml\",\"label\":\"spam\"}\n{\"path\":\"mail.eml\",\"label\":\"ham\"}\n"
	if err := os.WriteFile(corpus, []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	fake := &fakeEvaluator{}
	pauses := 0
	err := run(context.Background(), []string{"-corpus", corpus}, "secret", &output, &diagnostics, fake, func(context.Context, time.Duration) error { pauses++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 3 || pauses != 2 {
		t.Fatalf("calls=%d pauses=%d", fake.calls, pauses)
	}
	var total summary
	if err := json.Unmarshal(diagnostics.Bytes(), &total); err != nil {
		t.Fatal(err)
	}
	if total.Evaluated != 2 || total.Ham != 1 || total.Unscored != 1 || total.TruePositives != 1 || total.HamFalsePositiveRate == nil || *total.HamFalsePositiveRate != 0 || *total.Recall != 1 || total.InputTokens != 200 {
		t.Fatalf("summary: %+v", total)
	}
	decoder := json.NewDecoder(&output)
	for i := 0; i < 3; i++ {
		var row prediction
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err)
		}
		if i == 2 && (row.Spam != nil || row.Phishing != nil || row.WouldJunk != nil) {
			t.Fatal("error counted as negative")
		}
	}
	if strings.Contains(diagnostics.String(), "secret") {
		t.Fatal("key leaked")
	}
}

func TestEvaluationRequiresExplicitCorpusCredentialsAndValidOptions(t *testing.T) {
	for _, tc := range []struct {
		args []string
		key  string
	}{{nil, "secret"}, {[]string{"-corpus", "x"}, ""}, {[]string{"-corpus", "x", "-spam-threshold", "NaN"}, "secret"}, {[]string{"-corpus", "x", "-timeout-ms", "0"}, "secret"}} {
		fake := &fakeEvaluator{}
		var output bytes.Buffer
		if err := run(context.Background(), tc.args, tc.key, &output, &output, fake, wait); err == nil {
			t.Fatal("expected error")
		}
		if fake.calls != 0 {
			t.Fatal("invalid invocation made request")
		}
	}
	var output bytes.Buffer
	_ = run(context.Background(), []string{"-help"}, "private-key", &output, &output, nil, wait)
	if strings.Contains(output.String(), "private-key") || strings.Contains(output.String(), "-api-key") {
		t.Fatal("help exposed credentials")
	}
}

func TestCorpusValidationPrecedesInference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labels.jsonl")
	_ = os.WriteFile(path, []byte("{\"path\":\"message.eml\",\"label\":\"ham\"}\n{\"path\":\"message.eml\",\"label\":\"unknown\"}\n"), 0600)
	var output bytes.Buffer
	fake := &fakeEvaluator{}
	if err := run(context.Background(), []string{"-corpus", path}, "key", &output, &output, fake, wait); err == nil {
		t.Fatal("invalid label accepted")
	}
	if fake.calls != 0 {
		t.Fatal("called service before validating corpus")
	}
}
